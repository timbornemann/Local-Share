package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// eventKeepAliveInterval is how often idle event streams get a comment line.
var eventKeepAliveInterval = 25 * time.Second

const (
	defaultMaxFileSize  = 1 << 30 // 1 GiB
	defaultMaxTotalSize = 2 << 30 // 2 GiB
	defaultMaxItems     = 200
	defaultMaxTextSize  = 5 << 20 // 5 MiB

	maxPasswordLength   = 1024
	maxMultipartParts   = 300
	maxSubscribers      = 256
	maxUnlockBodyBytes  = 16 << 10
	maxJSONOverheadByte = 64 << 10
)

var (
	errFileTooLarge = errors.New("file exceeds the size limit")
	errStorageFull  = errors.New("not enough free share storage")
	errTooManyItems = errors.New("too many active shares")
)

var (
	errBlankPassword   = errors.New("password must not be only whitespace")
	errPasswordTooLong = fmt.Errorf("password must be at most %d bytes", maxPasswordLength)
)

const (
	kindFile    = "file"
	kindText    = "text"
	maxShareTTL = 24 * time.Hour
)

type Config struct {
	Addr            string
	TempDir         string
	TTL             time.Duration
	CleanupInterval time.Duration
	StaticFS        fs.FS
	Now             func() time.Time

	// Limits protect the host from being filled up by uploads. Zero selects the
	// default, a negative value disables the limit.
	MaxFileSize  int64 // bytes per file
	MaxTotalSize int64 // bytes across all active shares
	MaxItems     int   // number of active shares
	MaxTextSize  int64 // bytes per text share

	// AllowedHosts lists extra Host header values accepted besides IP
	// addresses, localhost and local-only names. See hostAllowed.
	AllowedHosts []string
}

type item struct {
	ID           string
	Kind         string
	Name         string
	Size         int64
	ContentType  string
	Path         string
	PasswordSalt []byte
	PasswordHash []byte
	AccessTokens map[string]time.Time
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type itemDTO struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	Protected   bool      `json:"protected"`
	Previewable bool      `json:"previewable"`
	PreviewKind string    `json:"previewKind"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type shareOptions struct {
	TTL      time.Duration
	Password string
}

type Server struct {
	ttl             time.Duration
	cleanupInterval time.Duration
	tempDir         string
	now             func() time.Time
	indexHTML       []byte
	staticHandler   http.Handler
	allowedHosts    []string

	maxFileSize  int64
	maxTotalSize int64
	maxItems     int
	maxTextSize  int64

	mu      sync.RWMutex
	items   map[string]*item
	used    int64 // bytes on disk for active shares plus uploads in progress
	pending int   // uploads in progress, counted against maxItems

	subsMu      sync.Mutex
	subscribers map[chan string]struct{}

	// Unlock attempts are limited per item and client, and per item overall.
	unlockByClient *failureLimiter
	unlockByItem   *failureLimiter

	stopOnce sync.Once
	stopping chan struct{}
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
	cfg.MaxFileSize = limitOrDefault(cfg.MaxFileSize, defaultMaxFileSize)
	cfg.MaxTotalSize = limitOrDefault(cfg.MaxTotalSize, defaultMaxTotalSize)
	cfg.MaxTextSize = limitOrDefault(cfg.MaxTextSize, defaultMaxTextSize)
	if cfg.MaxItems == 0 {
		cfg.MaxItems = defaultMaxItems
	}
	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return nil, err
	}
	removeStaleFiles(cfg.TempDir, cfg.Now().Add(-maxShareTTL))

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
		allowedHosts:    cfg.AllowedHosts,
		maxFileSize:     cfg.MaxFileSize,
		maxTotalSize:    cfg.MaxTotalSize,
		maxItems:        cfg.MaxItems,
		maxTextSize:     cfg.MaxTextSize,
		items:           make(map[string]*item),
		subscribers:     make(map[chan string]struct{}),
		stopping:        make(chan struct{}),
		unlockByClient:  newFailureLimiter(5, time.Minute),
		unlockByItem:    newFailureLimiter(60, 5*time.Minute),
	}, nil
}

// StopEvents ends all open event streams. Without it a connected browser tab
// would keep graceful shutdown waiting until its timeout.
func (s *Server) StopEvents() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

// Close ends event streams and deletes every stored file. Shares only live in
// memory, so their files are unreachable after shutdown and must not be left
// behind on disk.
func (s *Server) Close() {
	s.StopEvents()

	s.mu.Lock()
	paths := make([]string, 0, len(s.items))
	for id, it := range s.items {
		paths = append(paths, it.Path)
		s.forgetLocked(id, it)
	}
	s.mu.Unlock()

	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// removeStaleFiles deletes share files left behind by an earlier run that did
// not shut down cleanly. Nothing can outlive maxShareTTL, so anything older
// than that is unreachable no matter which instance created it.
func removeStaleFiles(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "share-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

func limitOrDefault(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", s.staticHandler))
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/items/files", s.handleUploadFiles)
	mux.HandleFunc("/api/items/text", s.handleCreateText)
	mux.HandleFunc("/api/items/", s.handleItemAction)
	mux.HandleFunc("/api/items", s.handleListItems)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/", s.handleIndex)
	return s.secure(mux)
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
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/items/") {
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

// handleConfig tells the UI which limits apply so it can refuse oversized
// files before sending them, instead of failing mid-upload.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		MaxFileSize  int64 `json:"maxFileSize"`
		MaxTotalSize int64 `json:"maxTotalSize"`
		MaxItems     int   `json:"maxItems"`
		MaxTextSize  int64 `json:"maxTextSize"`
		MaxTTL       int64 `json:"maxTtlSeconds"`
	}{
		MaxFileSize:  max(s.maxFileSize, 0),
		MaxTotalSize: max(s.maxTotalSize, 0),
		MaxItems:     max(s.maxItems, 0),
		MaxTextSize:  max(s.maxTextSize, 0),
		MaxTTL:       int64(maxShareTTL / time.Second),
	})
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
	committed := false
	defer func() {
		// A failed request must not leave half of a multi-file upload behind,
		// since the client only sees an error and cannot know what was stored.
		if committed {
			return
		}
		for _, dto := range created {
			s.deleteItem(dto.ID)
		}
	}()

	options := shareOptions{TTL: s.ttl}
	for parts := 0; ; parts++ {
		if parts >= maxMultipartParts {
			http.Error(w, "too many form parts", http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, "read multipart data", http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			value, err := readSmallPart(part)
			_ = part.Close()
			if err != nil {
				http.Error(w, "invalid upload option", http.StatusBadRequest)
				return
			}
			name := part.FormName()
			if (name == "password" || name == "ttlSeconds") && len(created) > 0 {
				// Options only apply to files that follow them. Accepting them
				// late would silently leave earlier files unprotected.
				http.Error(w, name+" must be sent before files", http.StatusBadRequest)
				return
			}
			switch name {
			case "password":
				if err := validatePassword(value); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				options.Password = value
			case "ttlSeconds":
				ttl, err := parseTTLSeconds(value, s.ttl)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				options.TTL = ttl
			}
			continue
		}
		if part.FormName() != "files" {
			_ = part.Close()
			continue
		}

		dto, err := s.storeStream(kindFile, part.FileName(), part.Header.Get("Content-Type"), part, options)
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

	committed = true
	s.broadcast("created")
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleCreateText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}

	var req struct {
		Text       string `json:"text"`
		Name       string `json:"name"`
		Password   string `json:"password"`
		TTLSeconds int64  `json:"ttlSeconds"`
	}
	body := io.Reader(r.Body)
	if s.maxTextSize > 0 {
		// JSON escaping can inflate text up to six times (control characters).
		body = http.MaxBytesReader(w, r.Body, s.maxTextSize*6+maxJSONOverheadByte)
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		if isBodyTooLarge(err) {
			writeStorageError(w, errFileTooLarge)
			return
		}
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if s.maxTextSize > 0 && int64(len(req.Text)) > s.maxTextSize {
		writeStorageError(w, errFileTooLarge)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	if err := validatePassword(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	name := cleanDisplayName(req.Name, "Text snippet")
	ttl, err := parseTTLSeconds(strconv.FormatInt(req.TTLSeconds, 10), s.ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dto, err := s.storeStream(kindText, name, "text/plain; charset=utf-8", strings.NewReader(req.Text), shareOptions{
		TTL:      ttl,
		Password: req.Password,
	})
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

	if len(parts) == 1 && r.Method == http.MethodGet {
		if s.cleanupExpired() {
			s.broadcast("expired")
		}
		it, ok := s.getActiveItem(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, toDTO(it))
		return
	}

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

	if len(parts) == 2 && parts[1] == "unlock" && r.Method == http.MethodPost {
		s.handleUnlock(w, r, id)
		return
	}

	if len(parts) == 2 && parts[1] == "raw" && r.Method == http.MethodGet {
		s.handleDownload(w, r, id, false)
		return
	}

	if len(parts) == 2 && parts[1] == "view" && r.Method == http.MethodGet {
		s.handleView(w, r, id)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request, id string) {
	if s.cleanupExpired() {
		s.broadcast("expired")
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUnlockBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	now := s.now().UTC()
	clientKey := id + "|" + clientAddr(r)
	wait := max(s.unlockByClient.retryAfter(clientKey, now), s.unlockByItem.retryAfter(id, now))
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		http.Error(w, fmt.Sprintf("too many wrong passwords, try again in %d seconds", int(wait/time.Second)+1), http.StatusTooManyRequests)
		return
	}

	token, expiresAt, status, err := s.createAccessToken(id, req.Password)
	if err != nil {
		http.Error(w, "could not unlock item", http.StatusInternalServerError)
		return
	}
	switch status {
	case http.StatusOK:
		s.unlockByClient.reset(clientKey)
		writeJSON(w, http.StatusOK, struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
		}{Token: token, ExpiresAt: expiresAt})
	case http.StatusNotFound:
		http.NotFound(w, r)
	case http.StatusBadRequest:
		http.Error(w, "item is not password protected", http.StatusBadRequest)
	default:
		s.unlockByClient.recordFailure(clientKey, now)
		s.unlockByItem.recordFailure(id, now)
		http.Error(w, "invalid password", http.StatusUnauthorized)
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, id string, attachment bool) {
	if s.cleanupExpired() {
		s.broadcast("expired")
	}

	it, ok := s.requireReadableItem(w, r, id)
	if !ok {
		return
	}
	if !attachment && !isTextPreviewable(it) {
		http.Error(w, "preview is not available for this file type", http.StatusUnsupportedMediaType)
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
	if !attachment {
		contentType = "text/plain; charset=utf-8"
	}
	setContentHeaders(w, contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", it.Size))
	if attachment {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
			"filename": cleanDisplayName(it.Name, "download"),
		}))
	}
	http.ServeContent(w, r, it.Name, it.CreatedAt, file)
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request, id string) {
	if s.cleanupExpired() {
		s.broadcast("expired")
	}

	it, ok := s.requireReadableItem(w, r, id)
	if !ok {
		return
	}
	kind := previewKindFor(it)
	if kind == "" {
		http.Error(w, "preview is not available for this file type", http.StatusUnsupportedMediaType)
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
	switch kind {
	case "text":
		contentType = "text/plain; charset=utf-8"
	case "pdf":
		contentType = "application/pdf"
	default:
		// The type is chosen by the uploader. Only pass it through inline when it
		// really is of the previewed family; otherwise derive it from the name.
		if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || !strings.HasPrefix(strings.ToLower(mediaType), kind+"/") {
			if guessed := mime.TypeByExtension(strings.ToLower(filepath.Ext(it.Name))); strings.HasPrefix(guessed, kind+"/") {
				contentType = guessed
			}
		}
	}
	setContentHeaders(w, contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", it.Size))
	http.ServeContent(w, r, it.Name, it.CreatedAt, file)
}

// setContentHeaders sets the headers shared by every response that serves
// stored user content. Uploaded files are untrusted: they are served from the
// app's own origin, so they must never be sniffed into something executable,
// and protected content must not end up in browser or proxy caches.
func setContentHeaders(w http.ResponseWriter, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	// Stored files must never run anything in the app's origin, whatever their
	// type claims to be (SVG and HTML can carry scripts). Opening one directly
	// gets an empty, sandboxed document; <img>/<video> rendering is unaffected.
	// PDFs are the exception: Chrome's viewer does not work inside a sandbox,
	// and PDF scripts do not run in the page's origin anyway.
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil && strings.EqualFold(mediaType, "application/pdf") {
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		return
	}
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox; frame-ancestors 'self'")
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
	w.Header().Set("X-Accel-Buffering", "no")

	ch, ok := s.subscribe()
	if !ok {
		http.Error(w, "too many open event streams", http.StatusServiceUnavailable)
		return
	}
	defer s.unsubscribe(ch)

	writeSSE(w, "connected", "ready")
	flusher.Flush()

	keepAlive := time.NewTicker(eventKeepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-keepAlive.C:
			// A comment line keeps idle connections from being dropped by
			// proxies and lets us notice clients that vanished silently.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case reason := <-ch:
			writeSSE(w, "items_changed", reason)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.stopping:
			return
		}
	}
}

func (s *Server) storeStream(kind, name, contentType string, reader io.Reader, options shareOptions) (itemDTO, error) {
	id, err := randomID()
	if err != nil {
		return itemDTO{}, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if options.TTL <= 0 {
		options.TTL = s.ttl
	}
	name = cleanDisplayName(name, "download")

	if err := s.reserveSlot(); err != nil {
		return itemDTO{}, err
	}
	defer s.releaseSlot()

	tmp, err := os.CreateTemp(s.tempDir, "share-*")
	if err != nil {
		return itemDTO{}, err
	}
	tmpPath := tmp.Name()
	quota := &quotaWriter{dst: tmp, srv: s}
	_, copyErr := io.Copy(quota, reader)
	closeErr := tmp.Close()
	written := quota.written
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		s.releaseBytes(written)
		if copyErr != nil {
			return itemDTO{}, copyErr
		}
		return itemDTO{}, closeErr
	}

	salt, hash, err := newPasswordDigest(options.Password)
	if err != nil {
		_ = os.Remove(tmpPath)
		s.releaseBytes(written)
		return itemDTO{}, err
	}

	now := s.now().UTC()
	it := &item{
		ID:           id,
		Kind:         kind,
		Name:         name,
		Size:         written,
		ContentType:  contentType,
		Path:         tmpPath,
		PasswordSalt: salt,
		PasswordHash: hash,
		AccessTokens: make(map[string]time.Time),
		CreatedAt:    now,
		ExpiresAt:    now.Add(options.TTL),
	}

	s.mu.Lock()
	s.items[id] = it
	s.mu.Unlock()

	return toDTO(it), nil
}

func (s *Server) requireReadableItem(w http.ResponseWriter, r *http.Request, id string) (*item, bool) {
	it, ok := s.getActiveItem(id)
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	if !isProtectedItem(it) || s.hasValidAccessToken(id, accessTokenFromRequest(r)) {
		return it, true
	}
	http.Error(w, "password required", http.StatusUnauthorized)
	return nil, false
}

func (s *Server) createAccessToken(id, password string) (string, time.Time, int, error) {
	now := s.now().UTC()

	// Verifying a password is deliberately slow, so do it without holding the
	// lock that every other request needs.
	s.mu.RLock()
	it, ok := s.items[id]
	if !ok || !it.ExpiresAt.After(now) {
		s.mu.RUnlock()
		return "", time.Time{}, http.StatusNotFound, nil
	}
	if !isProtectedItem(it) {
		s.mu.RUnlock()
		return "", time.Time{}, http.StatusBadRequest, nil
	}
	salt := append([]byte(nil), it.PasswordSalt...)
	want := append([]byte(nil), it.PasswordHash...)
	s.mu.RUnlock()

	if !passwordMatches(salt, want, password) {
		return "", time.Time{}, http.StatusUnauthorized, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok = s.items[id]
	if !ok || !it.ExpiresAt.After(now) {
		return "", time.Time{}, http.StatusNotFound, nil
	}

	token, err := randomID()
	if err != nil {
		return "", time.Time{}, http.StatusInternalServerError, err
	}
	expiresAt := it.ExpiresAt
	if it.AccessTokens == nil {
		it.AccessTokens = make(map[string]time.Time)
	}
	pruneAccessTokens(it, now)
	it.AccessTokens[token] = expiresAt
	return token, expiresAt, http.StatusOK, nil
}

func (s *Server) hasValidAccessToken(id, token string) bool {
	if token == "" {
		return false
	}

	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[id]
	if !ok || !it.ExpiresAt.After(now) {
		return false
	}
	pruneAccessTokens(it, now)
	expiresAt, ok := it.AccessTokens[token]
	return ok && expiresAt.After(now)
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
	copy.PasswordSalt = append([]byte(nil), it.PasswordSalt...)
	copy.PasswordHash = append([]byte(nil), it.PasswordHash...)
	copy.AccessTokens = nil
	s.mu.RUnlock()
	return &copy, true
}

func (s *Server) deleteItem(id string) bool {
	s.mu.Lock()
	it, ok := s.items[id]
	if ok {
		s.forgetLocked(id, it)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	_ = os.Remove(it.Path)
	return true
}

// forgetLocked drops an item from the index and returns its bytes to the
// storage budget. The caller removes the file. s.mu must be held.
func (s *Server) forgetLocked(id string, it *item) {
	delete(s.items, id)
	s.used -= it.Size
	if s.used < 0 {
		s.used = 0
	}
}

func (s *Server) reserveSlot() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxItems > 0 && len(s.items)+s.pending >= s.maxItems {
		return errTooManyItems
	}
	s.pending++
	return nil
}

func (s *Server) releaseSlot() {
	s.mu.Lock()
	s.pending--
	s.mu.Unlock()
}

func (s *Server) releaseBytes(n int64) {
	s.mu.Lock()
	s.used -= n
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

// quotaWriter enforces the per-file and total storage limits while a file is
// streamed to disk, so an oversized upload is cut off as soon as it crosses a
// limit instead of after it has filled the disk.
type quotaWriter struct {
	dst     io.Writer
	srv     *Server
	written int64
}

func (q *quotaWriter) Write(p []byte) (int, error) {
	n := int64(len(p))
	if q.srv.maxFileSize > 0 && q.written+n > q.srv.maxFileSize {
		return 0, errFileTooLarge
	}
	q.srv.mu.Lock()
	if q.srv.maxTotalSize > 0 && q.srv.used+n > q.srv.maxTotalSize {
		q.srv.mu.Unlock()
		return 0, errStorageFull
	}
	q.srv.used += n
	q.srv.mu.Unlock()
	q.written += n

	wrote, err := q.dst.Write(p)
	if int64(wrote) < n {
		// Keep the accounting exact if the disk accepted only part of the write.
		q.srv.releaseBytes(n - int64(wrote))
		q.written -= n - int64(wrote)
	}
	return wrote, err
}

func (s *Server) cleanupExpired() bool {
	now := s.now().UTC()
	s.unlockByClient.prune(now)
	s.unlockByItem.prune(now)
	var paths []string

	s.mu.Lock()
	for id, it := range s.items {
		if !it.ExpiresAt.After(now) {
			paths = append(paths, it.Path)
			s.forgetLocked(id, it)
			continue
		}
		pruneAccessTokens(it, now)
	}
	s.mu.Unlock()

	for _, p := range paths {
		_ = os.Remove(p)
	}
	return len(paths) > 0
}

func (s *Server) subscribe() (chan string, bool) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if len(s.subscribers) >= maxSubscribers {
		return nil, false
	}
	ch := make(chan string, 8)
	s.subscribers[ch] = struct{}{}
	return ch, true
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
	previewKind := previewKindFor(it)
	return itemDTO{
		ID:          it.ID,
		Kind:        it.Kind,
		Name:        it.Name,
		Size:        it.Size,
		ContentType: it.ContentType,
		Protected:   isProtectedItem(it),
		Previewable: previewKind != "",
		PreviewKind: previewKind,
		CreatedAt:   it.CreatedAt,
		ExpiresAt:   it.ExpiresAt,
	}
}

func isTextPreviewable(it *item) bool {
	return previewKindFor(it) == "text"
}

func previewKindFor(it *item) string {
	if it == nil {
		return ""
	}
	if it.Kind == kindText {
		return "text"
	}

	mediaType, _, err := mime.ParseMediaType(it.ContentType)
	if err == nil {
		mediaType = strings.ToLower(mediaType)
		if strings.HasPrefix(mediaType, "text/") {
			return "text"
		}
		switch {
		case strings.HasPrefix(mediaType, "image/"):
			return "image"
		case strings.HasPrefix(mediaType, "audio/"):
			return "audio"
		case strings.HasPrefix(mediaType, "video/"):
			return "video"
		case mediaType == "application/pdf":
			return "pdf"
		}
		switch mediaType {
		case "application/json", "application/xml", "application/yaml", "application/x-yaml",
			"application/toml", "application/javascript", "application/ecmascript",
			"application/x-javascript", "application/sql", "application/graphql":
			return "text"
		}
		if strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
			return "text"
		}
	}

	name := strings.ToLower(it.Name)
	if name == "dockerfile" || name == "makefile" || name == ".env" {
		return "text"
	}
	switch filepath.Ext(name) {
	case ".txt", ".text", ".md", ".markdown", ".log", ".csv", ".tsv",
		".json", ".jsonl", ".xml", ".yaml", ".yml", ".toml", ".ini", ".env",
		".html", ".htm", ".css", ".scss", ".sass", ".less",
		".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx",
		".go", ".rs", ".py", ".rb", ".php", ".java", ".kt", ".kts", ".swift",
		".c", ".h", ".cpp", ".hpp", ".cc", ".cs", ".fs", ".fsx",
		".sh", ".bash", ".zsh", ".fish", ".ps1", ".bat", ".cmd",
		".sql", ".graphql", ".gql", ".dockerignore", ".gitignore":
		return "text"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".avif", ".svg":
		return "image"
	case ".pdf":
		return "pdf"
	case ".mp3", ".wav", ".ogg", ".m4a", ".flac", ".aac":
		return "audio"
	case ".mp4", ".webm", ".mov", ".m4v", ".ogv":
		return "video"
	default:
		return ""
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newPasswordDigest(password string) ([]byte, []byte, error) {
	password = strings.TrimSpace(password)
	if password == "" {
		return nil, nil, nil
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	hash, err := hashPassword(salt, password)
	if err != nil {
		return nil, nil, err
	}
	return salt, hash, nil
}

// passwordIterations makes every password guess cost real CPU time. The hashes
// only live in memory, but a slow KDF also throttles online guessing and keeps
// a leaked process dump from being cheap to crack.
const passwordIterations = 120_000

func hashPassword(salt []byte, password string) ([]byte, error) {
	return pbkdf2.Key(sha256.New, strings.TrimSpace(password), salt, passwordIterations, sha256.Size)
}

func passwordMatches(salt, want []byte, password string) bool {
	got, err := hashPassword(salt, password)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func isProtectedItem(it *item) bool {
	return it != nil && len(it.PasswordHash) > 0
}

func pruneAccessTokens(it *item, now time.Time) {
	for token, expiresAt := range it.AccessTokens {
		if !expiresAt.After(now) {
			delete(it.AccessTokens, token)
		}
	}
}

func accessTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		return token
	}
	return strings.TrimSpace(r.Header.Get("X-Share-Token"))
}

func readSmallPart(reader io.Reader) (string, error) {
	const maxOptionBytes = 8 << 10
	data, err := io.ReadAll(io.LimitReader(reader, maxOptionBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxOptionBytes {
		return "", errors.New("upload option is too large")
	}
	return string(data), nil
}

// validatePassword rejects passwords that consist only of whitespace. They are
// trimmed to nothing when hashed, which would silently create an unprotected
// share even though the sender asked for a password.
func validatePassword(password string) error {
	if password != "" && strings.TrimSpace(password) == "" {
		return errBlankPassword
	}
	if len(password) > maxPasswordLength {
		return errPasswordTooLong
	}
	return nil
}

func parseTTLSeconds(value string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if fallback <= 0 {
		fallback = 5 * time.Minute
	}
	if value == "" || value == "0" {
		return fallback, nil
	}

	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 1 || seconds > int64(maxShareTTL/time.Second) {
		return 0, fmt.Errorf("ttlSeconds must be between 1 and %d", int64(maxShareTTL/time.Second))
	}
	return time.Duration(seconds) * time.Second, nil
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
	if value == "" || value == "." || value == ".." || value == string(filepath.Separator) {
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

func isBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errFileTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, errStorageFull), errors.Is(err, errTooManyItems):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	}
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
