package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	kindFile = "file"
	kindText = "text"
)

type Config struct {
	Addr            string
	TempDir         string
	TTL             time.Duration
	CleanupInterval time.Duration
	StaticFS        fs.FS
	Now             func() time.Time
}

type item struct {
	ID          string
	Kind        string
	Name        string
	Size        int64
	ContentType string
	Path        string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type itemDTO struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type Server struct {
	ttl             time.Duration
	cleanupInterval time.Duration
	tempDir         string
	now             func() time.Time
	indexHTML       []byte
	staticHandler   http.Handler

	mu    sync.RWMutex
	items map[string]*item

	subsMu      sync.Mutex
	subscribers map[chan string]struct{}
}

func NewServer(cfg Config) (*Server, error) {
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 5 * time.Second
	}
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(os.TempDir(), "local-share")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StaticFS == nil {
		cfg.StaticFS = webFS
	}
	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return nil, err
	}

	static, err := fs.Sub(cfg.StaticFS, "web")
	if err != nil {
		return nil, err
	}
	indexHTML, err := fs.ReadFile(static, "index.html")
	if err != nil {
		return nil, err
	}

	return &Server{
		ttl:             cfg.TTL,
		cleanupInterval: cfg.CleanupInterval,
		tempDir:         cfg.TempDir,
		now:             cfg.Now,
		indexHTML:       indexHTML,
		staticHandler:   http.FileServer(http.FS(static)),
		items:           make(map[string]*item),
		subscribers:     make(map[chan string]struct{}),
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", s.staticHandler))
	mux.HandleFunc("/api/items/files", s.handleUploadFiles)
	mux.HandleFunc("/api/items/text", s.handleCreateText)
	mux.HandleFunc("/api/items/", s.handleItemAction)
	mux.HandleFunc("/api/items", s.handleListItems)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func (s *Server) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.cleanupExpired() {
				s.broadcast("expired")
			}
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(s.indexHTML)
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/items" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	if s.cleanupExpired() {
		s.broadcast("expired")
	}

	s.mu.RLock()
	items := make([]itemDTO, 0, len(s.items))
	for _, it := range s.items {
		items = append(items, toDTO(it))
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart form data", http.StatusBadRequest)
		return
	}

	created := make([]itemDTO, 0)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, "read multipart data", http.StatusBadRequest)
			return
		}
		if part.FormName() != "files" || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		dto, err := s.storeStream(kindFile, part.FileName(), part.Header.Get("Content-Type"), part)
		_ = part.Close()
		if err != nil {
			writeStorageError(w, err)
			return
		}
		created = append(created, dto)
	}

	if len(created) == 0 {
		http.Error(w, "no files uploaded", http.StatusBadRequest)
		return
	}

	s.broadcast("created")
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleCreateText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	var req struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	name := cleanDisplayName(req.Name, "Text snippet")
	dto, err := s.storeStream(kindText, name, "text/plain; charset=utf-8", strings.NewReader(req.Text))
	if err != nil {
		writeStorageError(w, err)
		return
	}

	s.broadcast("created")
	writeJSON(w, http.StatusCreated, dto)
}

func (s *Server) handleItemAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/items/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]

	if len(parts) == 1 && r.Method == http.MethodDelete {
		if s.cleanupExpired() {
			s.broadcast("expired")
		}
		if !s.deleteItem(id) {
			http.NotFound(w, r)
			return
		}
		s.broadcast("deleted")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodGet {
		s.handleDownload(w, r, id, true)
		return
	}

	if len(parts) == 2 && parts[1] == "raw" && r.Method == http.MethodGet {
		s.handleDownload(w, r, id, false)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, id string, attachment bool) {
	if s.cleanupExpired() {
		s.broadcast("expired")
	}

	it, ok := s.getActiveItem(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !attachment && it.Kind != kindText {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(it.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	contentType := it.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", it.Size))
	if attachment {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
			"filename": cleanDisplayName(it.Name, "download"),
		}))
	}
	http.ServeContent(w, r, it.Name, it.CreatedAt, file)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.subscribe()
	defer s.unsubscribe(ch)

	writeSSE(w, "connected", "ready")
	flusher.Flush()

	for {
		select {
		case reason := <-ch:
			writeSSE(w, "items_changed", reason)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) storeStream(kind, name, contentType string, reader io.Reader) (itemDTO, error) {
	id, err := randomID()
	if err != nil {
		return itemDTO{}, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	name = cleanDisplayName(name, "download")

	tmp, err := os.CreateTemp(s.tempDir, "share-*")
	if err != nil {
		return itemDTO{}, err
	}
	tmpPath := tmp.Name()
	written, copyErr := io.Copy(tmp, reader)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		if copyErr != nil {
			return itemDTO{}, copyErr
		}
		return itemDTO{}, closeErr
	}

	now := s.now().UTC()
	it := &item{
		ID:          id,
		Kind:        kind,
		Name:        name,
		Size:        written,
		ContentType: contentType,
		Path:        tmpPath,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.ttl),
	}

	s.mu.Lock()
	s.items[id] = it
	s.mu.Unlock()

	return toDTO(it), nil
}

func (s *Server) getActiveItem(id string) (*item, bool) {
	now := s.now().UTC()
	s.mu.RLock()
	it, ok := s.items[id]
	if !ok || !it.ExpiresAt.After(now) {
		s.mu.RUnlock()
		return nil, false
	}
	copy := *it
	s.mu.RUnlock()
	return &copy, true
}

func (s *Server) deleteItem(id string) bool {
	s.mu.Lock()
	it, ok := s.items[id]
	if ok {
		delete(s.items, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	_ = os.Remove(it.Path)
	return true
}

func (s *Server) cleanupExpired() bool {
	now := s.now().UTC()
	var paths []string

	s.mu.Lock()
	for id, it := range s.items {
		if !it.ExpiresAt.After(now) {
			paths = append(paths, it.Path)
			delete(s.items, id)
		}
	}
	s.mu.Unlock()

	for _, p := range paths {
		_ = os.Remove(p)
	}
	return len(paths) > 0
}

func (s *Server) subscribe() chan string {
	ch := make(chan string, 8)
	s.subsMu.Lock()
	s.subscribers[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan string) {
	s.subsMu.Lock()
	delete(s.subscribers, ch)
	close(ch)
	s.subsMu.Unlock()
}

func (s *Server) broadcast(reason string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- reason:
		default:
		}
	}
}

func toDTO(it *item) itemDTO {
	return itemDTO{
		ID:          it.ID,
		Kind:        it.Kind,
		Name:        it.Name,
		Size:        it.Size,
		ContentType: it.ContentType,
		CreatedAt:   it.CreatedAt,
		ExpiresAt:   it.ExpiresAt,
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func cleanDisplayName(value, fallback string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Base(value)
	value = filepath.Base(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == string(filepath.Separator) {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSSE(w io.Writer, event, data string) {
	data = strings.ReplaceAll(data, "\r", "")
	lines := strings.Split(data, "\n")
	_, _ = fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range lines {
		_, _ = fmt.Fprintf(w, "data: %s\n", line)
	}
	_, _ = fmt.Fprint(w, "\n")
}

func writeStorageError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrPermission) {
		http.Error(w, "storage is not writable", http.StatusInternalServerError)
		return
	}
	http.Error(w, "could not store content", http.StatusInsufficientStorage)
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
