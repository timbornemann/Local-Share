package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
)

type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time {
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func newTestServer(t *testing.T, ttl time.Duration) (*Server, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)}
	srv, err := NewServer(Config{
		TempDir:         t.TempDir(),
		TTL:             ttl,
		CleanupInterval: time.Hour,
		StaticFS:        webFS,
		Now:             clock.Now,
		AllowedHosts:    []string{"*"}, // httptest requests use Host example.com
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, clock
}

func TestFileUploadAndBinaryDownload(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	payload := []byte{0x00, 0x01, 0xff, 0x42, 0x0a}

	created := uploadFiles(t, handler, map[string][]byte{"sample.bin": payload})
	if len(created) != 1 {
		t.Fatalf("expected 1 item, got %d", len(created))
	}
	if created[0].Kind != kindFile || created[0].Name != "sample.bin" || created[0].Size != int64(len(payload)) {
		t.Fatalf("unexpected metadata: %+v", created[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/download", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("download body mismatch: %v", rec.Body.Bytes())
	}
}

func TestMultipleFileUpload(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	created := uploadFiles(t, srv.Routes(), map[string][]byte{
		"a.txt": []byte("alpha"),
		"b.zip": []byte("bravo"),
	})

	if len(created) != 2 {
		t.Fatalf("expected 2 uploaded items, got %d", len(created))
	}
	names := map[string]bool{created[0].Name: true, created[1].Name: true}
	if !names["a.txt"] || !names["b.zip"] {
		t.Fatalf("unexpected uploaded names: %+v", created)
	}
}

func TestTextShareRawAndDownload(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	text := "hello from the clipboard\nsecond line"

	created := createText(t, handler, "note.txt", text)
	if created.Kind != kindText || created.Name != "note.txt" || created.Size != int64(len(text)) {
		t.Fatalf("unexpected metadata: %+v", created)
	}
	if !created.Previewable || created.PreviewKind != "text" {
		t.Fatalf("text item should be previewable: %+v", created)
	}

	for _, route := range []string{"/raw", "/download"} {
		req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", route, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != text {
			t.Fatalf("%s body mismatch: %q", route, rec.Body.String())
		}
	}
}

func TestProtectedTextRequiresUnlockForRawViewAndDownload(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	text := "secret clipboard value"

	created := createTextWithOptions(t, handler, "secret.txt", text, 90, "open-sesame")
	if !created.Protected {
		t.Fatalf("text item should be protected: %+v", created)
	}
	if got := created.ExpiresAt.Sub(created.CreatedAt); got != 90*time.Second {
		t.Fatalf("custom ttl = %s, want 90s", got)
	}

	for _, route := range []string{"/raw", "/view", "/download"} {
		req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token status = %d, body = %s", route, rec.Code, rec.Body.String())
		}
	}

	unlockItemForTest(t, handler, created.ID, "wrong-password", http.StatusUnauthorized)
	unlocked := unlockItemForTest(t, handler, created.ID, "open-sesame", http.StatusOK)
	if unlocked.Token == "" {
		t.Fatalf("unlock response should include a token")
	}
	if !unlocked.ExpiresAt.Equal(created.ExpiresAt) {
		t.Fatalf("unlock expiry = %s, want item expiry %s", unlocked.ExpiresAt, created.ExpiresAt)
	}

	for _, route := range []string{"/raw", "/view", "/download"} {
		req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+route+"?token="+unlocked.Token, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s with token status = %d, body = %s", route, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != text {
			t.Fatalf("%s body mismatch: %q", route, rec.Body.String())
		}
	}
}

func TestProtectedFileDownloadRequiresUnlock(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	payload := []byte{0x50, 0x51, 0x52, 0x00}

	created := uploadFilesWithOptions(t, handler, map[string][]byte{"secret.bin": payload}, testUploadOptions{
		TTLSeconds: 120,
		Password:   "download-me",
	})
	if len(created) != 1 {
		t.Fatalf("expected 1 item, got %d", len(created))
	}
	if !created[0].Protected {
		t.Fatalf("file item should be protected: %+v", created[0])
	}
	if got := created[0].ExpiresAt.Sub(created[0].CreatedAt); got != 120*time.Second {
		t.Fatalf("custom ttl = %s, want 120s", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/download", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("download without token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	unlocked := unlockItemForTest(t, handler, created[0].ID, "download-me", http.StatusOK)
	req = httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/download?token="+unlocked.Token, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download with token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("download body mismatch: %v", rec.Body.Bytes())
	}
}

func TestCustomTTLExpiresItem(t *testing.T) {
	srv, clock := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createTextWithOptions(t, handler, "short", "brief", 30, "")

	clock.Advance(31 * time.Second)

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+"/download", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download after custom ttl status = %d", rec.Code)
	}
}

func TestInvalidTTLIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()

	reqBody := strings.NewReader(`{"name":"too-long","text":"x","ttlSeconds":86401}`)
	req := httptest.NewRequest(http.MethodPost, "/api/items/text", reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ttl status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGetItemMetadata(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createText(t, handler, "metadata", "details")

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got itemDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if got.ID != created.ID || got.Name != "metadata" || !got.Previewable {
		t.Fatalf("unexpected metadata: %+v", got)
	}
}

func TestCodeFileCanBePreviewedRaw(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	source := []byte("package main\n\nfunc main() {}\n")
	created := uploadFiles(t, handler, map[string][]byte{"main.go": source})
	if len(created) != 1 {
		t.Fatalf("expected 1 item, got %d", len(created))
	}
	if !created[0].Previewable || created[0].PreviewKind != "text" {
		t.Fatalf("code file should be previewable: %+v", created[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/raw", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("raw preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(source) {
		t.Fatalf("raw preview mismatch: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("raw preview content type = %q", got)
	}
}

func TestUnsupportedFileRawPreviewReturns415(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := uploadFiles(t, handler, map[string][]byte{"slides.pptx": []byte("not really pptx")})
	if len(created) != 1 {
		t.Fatalf("expected 1 item, got %d", len(created))
	}
	if created[0].Previewable {
		t.Fatalf("pptx should not be previewable: %+v", created[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/raw", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("raw preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestOfficeContentTypeIsNotTextPreviewable(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := uploadFileWithContentType(
		t,
		handler,
		"slides.pptx",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		[]byte("pptx-ish"),
	)
	if created.Previewable || created.PreviewKind != "" {
		t.Fatalf("office file should not be previewable: %+v", created)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+"/raw", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("raw preview status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestImageFileCanBeViewedInline(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	created := uploadFiles(t, handler, map[string][]byte{"image.png": imageBytes})
	if len(created) != 1 {
		t.Fatalf("expected 1 item, got %d", len(created))
	}
	if !created[0].Previewable || created[0].PreviewKind != "image" {
		t.Fatalf("png should be image-previewable: %+v", created[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created[0].ID+"/view", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inline view status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageBytes) {
		t.Fatalf("inline view body mismatch: %v", rec.Body.Bytes())
	}
}

func TestItemDetailRouteServesApp(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	req := httptest.NewRequest(http.MethodGet, "/items/some-id", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail route status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Local Share") {
		t.Fatalf("detail route should serve the app shell")
	}
}

func TestDeleteRemovesItemAndFile(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createText(t, handler, "delete-me", "gone soon")

	srv.mu.RLock()
	storedPath := srv.items[created.ID].Path
	srv.mu.RUnlock()
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("stored file should exist: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/items/"+created.ID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(storedPath); !os.IsNotExist(err) {
		t.Fatalf("stored file should be removed, stat err = %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+"/download", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download after delete status = %d", rec.Code)
	}
}

func TestExpiryRemovesItemAndRejectsDownload(t *testing.T) {
	srv, clock := newTestServer(t, time.Minute)
	handler := srv.Routes()
	created := createText(t, handler, "expires", "time limited")

	srv.mu.RLock()
	storedPath := srv.items[created.ID].Path
	srv.mu.RUnlock()
	clock.Advance(time.Minute + time.Second)

	req := httptest.NewRequest(http.MethodGet, "/api/items/"+created.ID+"/download", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download after expiry status = %d", rec.Code)
	}
	if _, err := os.Stat(storedPath); !os.IsNotExist(err) {
		t.Fatalf("expired file should be removed, stat err = %v", err)
	}
}

func TestFailedUploadDoesNotLeaveStoredFilesBehind(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "first.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write([]byte("stored before the failure"))
	// An invalid option after a stored file makes the whole request fail.
	_ = writer.WriteField("ttlSeconds", "not-a-number")
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	srv.mu.RLock()
	remaining := len(srv.items)
	srv.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("failed upload left %d item(s) behind", remaining)
	}
	entries, err := os.ReadDir(srv.tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed upload left %d temp file(s) behind", len(entries))
	}
}

func TestCloseRemovesStoredFilesAndStopsEventStreams(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	createText(t, handler, "one", "first")
	uploadFiles(t, handler, map[string][]byte{"two.bin": {1, 2, 3}})

	httpSrv := httptest.NewServer(handler)
	defer httpSrv.Close()
	resp, err := http.Get(httpSrv.URL + "/api/events")
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer resp.Body.Close()
	streamEnded := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(streamEnded)
	}()

	srv.Close()
	srv.Close() // must be safe to call more than once

	select {
	case <-streamEnded:
	case <-time.After(2 * time.Second):
		t.Fatalf("event stream should end when the server closes")
	}
	entries, err := os.ReadDir(srv.tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Close left %d file(s) behind", len(entries))
	}
}

func TestNewServerRemovesStaleShareFiles(t *testing.T) {
	dir := t.TempDir()
	old := dir + string(os.PathSeparator) + "share-old"
	recent := dir + string(os.PathSeparator) + "share-recent"
	other := dir + string(os.PathSeparator) + "keep-me.txt"
	for _, p := range []string{old, recent, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(old, now, now.Add(-maxShareTTL-time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(other, now, now.Add(-maxShareTTL-time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := NewServer(Config{TempDir: dir, StaticFS: webFS}); err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale share file should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent share file must be kept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated file must be kept: %v", err)
	}
}

func TestStoredContentIsServedWithHardenedHeaders(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	svg := uploadFileWithContentType(t, handler, "logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))

	for _, route := range []string{"/view", "/download"} {
		req := httptest.NewRequest(http.MethodGet, "/api/items/"+svg.ID+route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", route, rec.Code)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q", route, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q", route, got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
			t.Fatalf("%s should sandbox SVG, CSP = %q", route, got)
		}
	}

	text := createText(t, handler, "note.txt", "plain")
	req := httptest.NewRequest(http.MethodGet, "/api/items/"+text.ID+"/raw", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("raw text is missing hardened headers: %v", rec.Header())
	}
}

func TestBlankPasswordIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/items/text", strings.NewReader(`{"text":"hi","password":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("text with blank password status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("password", "  ")
	part, _ := writer.CreateFormFile("files", "a.txt")
	_, _ = part.Write([]byte("data"))
	_ = writer.Close()
	req = httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload with blank password status = %d, body = %s", rec.Code, rec.Body.String())
	}

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if len(srv.items) != 0 {
		t.Fatalf("rejected requests created %d item(s)", len(srv.items))
	}
}

func TestUploadOptionsAfterFilesAreRejected(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("files", "a.txt")
	_, _ = part.Write([]byte("data"))
	_ = writer.WriteField("password", "late-secret")
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("late password status = %d, body = %s", rec.Code, rec.Body.String())
	}

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if len(srv.items) != 0 {
		t.Fatalf("rejected upload left %d item(s) behind", len(srv.items))
	}
}

func TestSSEEmitsItemChange(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	httpSrv := httptest.NewServer(srv.Routes())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/events")
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", resp.StatusCode)
	}

	events := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				events <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()

	waitForEvent(t, events, "connected")
	createTextHTTP(t, httpSrv.URL, "evented", "notify")
	waitForEvent(t, events, "items_changed")
}

func TestCleanDisplayName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":           "report.pdf",
		"../../etc/passwd":     "passwd",
		`C:\Users\me\file.txt`: "file.txt",
		"..":                   "fallback",
		".":                    "fallback",
		"":                     "fallback",
		"  \x00 ":              "fallback",
		"a\nb.txt":             "ab.txt",
	}
	for input, want := range cases {
		if got := cleanDisplayName(input, "fallback"); got != want {
			t.Errorf("cleanDisplayName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSSESendsKeepAlive(t *testing.T) {
	previous := eventKeepAliveInterval
	eventKeepAliveInterval = 20 * time.Millisecond
	defer func() { eventKeepAliveInterval = previous }()

	srv, _ := newTestServer(t, 5*time.Minute)
	httpSrv := httptest.NewServer(srv.Routes())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/events")
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer resp.Body.Close()

	found := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), ": keepalive") {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(2 * time.Second):
		t.Fatalf("no keepalive comment received")
	}
}

type testUploadOptions struct {
	TTLSeconds int64
	Password   string
}

func uploadFiles(t *testing.T, handler http.Handler, files map[string][]byte) []itemDTO {
	t.Helper()
	return uploadFilesWithOptions(t, handler, files, testUploadOptions{})
}

func uploadFilesWithOptions(t *testing.T, handler http.Handler, files map[string][]byte, options testUploadOptions) []itemDTO {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if options.TTLSeconds > 0 {
		if err := writer.WriteField("ttlSeconds", fmt.Sprintf("%d", options.TTLSeconds)); err != nil {
			t.Fatalf("write ttl field: %v", err)
		}
	}
	if options.Password != "" {
		if err := writer.WriteField("password", options.Password); err != nil {
			t.Fatalf("write password field: %v", err)
		}
	}
	for name, content := range files {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var created []itemDTO
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return created
}

func uploadFileWithContentType(t *testing.T, handler http.Handler, name, contentType string, content []byte) itemDTO {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename="%s"`, name))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var created []itemDTO
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 uploaded item, got %d", len(created))
	}
	return created[0]
}

func createText(t *testing.T, handler http.Handler, name, text string) itemDTO {
	t.Helper()
	return createTextWithOptions(t, handler, name, text, 0, "")
}

func createTextWithOptions(t *testing.T, handler http.Handler, name, text string, ttlSeconds int64, password string) itemDTO {
	t.Helper()
	reqBody := strings.NewReader(
		`{"name":` + quote(name) +
			`,"text":` + quote(text) +
			`,"ttlSeconds":` + fmt.Sprintf("%d", ttlSeconds) +
			`,"password":` + quote(password) +
			`}`,
	)
	req := httptest.NewRequest(http.MethodPost, "/api/items/text", reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create text status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var created itemDTO
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode text response: %v", err)
	}
	return created
}

type unlockResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func unlockItemForTest(t *testing.T, handler http.Handler, id, password string, wantStatus int) unlockResponse {
	t.Helper()
	reqBody := strings.NewReader(`{"password":` + quote(password) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/items/"+id+"/unlock", reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("unlock status = %d, want %d, body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	if wantStatus != http.StatusOK {
		return unlockResponse{}
	}
	var unlocked unlockResponse
	if err := json.NewDecoder(rec.Body).Decode(&unlocked); err != nil {
		t.Fatalf("decode unlock response: %v", err)
	}
	return unlocked
}

func createTextHTTP(t *testing.T, baseURL, name, text string) itemDTO {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/items/text", "application/json", strings.NewReader(`{"name":`+quote(name)+`,"text":`+quote(text)+`}`))
	if err != nil {
		t.Fatalf("post text: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post text status = %d, body = %s", resp.StatusCode, string(body))
	}
	var created itemDTO
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode text response: %v", err)
	}
	return created
}

func quote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func waitForEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-events:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for SSE event %q", want)
		}
	}
}

func TestInlineViewDoesNotTrustUploaderContentType(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	cases := []struct{ name, claimed, want string }{
		{"photo.png", "application/octet-stream", "image/png"},
		{"doc.pdf", "application/octet-stream", "application/pdf"},
	}
	for _, tc := range cases {
		item := uploadFileWithContentType(t, handler, tc.name, tc.claimed, []byte("data"))
		req := httptest.NewRequest(http.MethodGet, "/api/items/"+item.ID+"/view", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s view status = %d", tc.name, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.want {
			t.Errorf("%s served as %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHostAllowlist(t *testing.T) {
	cases := []struct {
		host  string
		extra []string
		want  bool
	}{
		{"localhost:8080", nil, true},
		{"127.0.0.1:8080", nil, true},
		{"192.168.1.20", nil, true},
		{"[::1]:8080", nil, true},
		{"[fe80::1]", nil, true},
		{"my-pc", nil, true},
		{"my-pc.local:8080", nil, true},
		{"nas.lan", nil, true},
		{"evil.example.com", nil, false},
		{"evil.example.com:8080", nil, false},
		{"", nil, false},
		{"share.example.com", []string{"share.example.com"}, true},
		{"a.example.com", []string{"*.example.com"}, true},
		{"example.com", []string{"*.example.com"}, false},
		{"anything.test", []string{"*"}, true},
	}
	for _, tc := range cases {
		if got := hostAllowed(tc.host, tc.extra); got != tc.want {
			t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.host, tc.extra, got, tc.want)
		}
	}
}

func newStrictTestServer(t *testing.T) http.Handler {
	t.Helper()
	srv, err := NewServer(Config{TempDir: t.TempDir(), CleanupInterval: time.Hour, StaticFS: webFS})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv.Routes()
}

func TestRebindingHostIsRejected(t *testing.T) {
	handler := newStrictTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	req.Host = "attacker.example.com:8080"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/items", nil)
	req.Host = "192.168.1.20:8080"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LAN host status = %d", rec.Code)
	}
}

func TestCrossSitePostsAreRejected(t *testing.T) {
	handler := newStrictTestServer(t)
	post := func(headers map[string]string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/items/text", strings.NewReader(`{"text":"hello"}`))
		req.Host = "192.168.1.20:8080"
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post(map[string]string{"Origin": "https://evil.example.com"}); got != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d", got)
	}
	if got := post(map[string]string{"Origin": "null"}); got != http.StatusForbidden {
		t.Fatalf("null-origin POST status = %d", got)
	}
	if got := post(map[string]string{"Sec-Fetch-Site": "cross-site"}); got != http.StatusForbidden {
		t.Fatalf("cross-site fetch-metadata POST status = %d", got)
	}
	if got := post(map[string]string{"Origin": "http://192.168.1.20:8080", "Sec-Fetch-Site": "same-origin"}); got != http.StatusCreated {
		t.Fatalf("same-origin POST status = %d", got)
	}
	if got := post(nil); got != http.StatusCreated {
		t.Fatalf("non-browser POST status = %d", got)
	}
}

func TestAppResponsesCarrySecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	for _, path := range []string{"/", "/api/items", "/static/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
			h.Get("X-Frame-Options") != "SAMEORIGIN" || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s is missing security headers: %v", path, h)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("API responses must not be cached")
	}
}

func newLimitedServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.TempDir = t.TempDir()
	cfg.CleanupInterval = time.Hour
	cfg.StaticFS = webFS
	cfg.AllowedHosts = []string{"*"}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func rawUpload(t *testing.T, handler http.Handler, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, content := range files {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		_, _ = part.Write(content)
	}
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/items/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func storedFileCount(t *testing.T, srv *Server) int {
	t.Helper()
	entries, err := os.ReadDir(srv.tempDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	return len(entries)
}

func TestOversizedFileIsRejectedAndLeavesNothingBehind(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxFileSize: 100})
	handler := srv.Routes()

	rec := rawUpload(t, handler, map[string][]byte{"big.bin": bytes.Repeat([]byte("x"), 101)})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if n := storedFileCount(t, srv); n != 0 {
		t.Fatalf("oversized upload left %d temp file(s)", n)
	}
	if srv.used != 0 {
		t.Fatalf("storage accounting leaked %d bytes", srv.used)
	}

	if rec := rawUpload(t, handler, map[string][]byte{"ok.bin": bytes.Repeat([]byte("x"), 100)}); rec.Code != http.StatusCreated {
		t.Fatalf("file at the limit status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTotalStorageLimitAndAccounting(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxTotalSize: 150})
	handler := srv.Routes()

	first := uploadFiles(t, handler, map[string][]byte{"a.bin": bytes.Repeat([]byte("a"), 100)})
	rec := rawUpload(t, handler, map[string][]byte{"b.bin": bytes.Repeat([]byte("b"), 100)})
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload beyond total limit status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if n := storedFileCount(t, srv); n != 1 {
		t.Fatalf("expected only the first file on disk, found %d", n)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/items/"+first[0].ID, nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if srv.used != 0 {
		t.Fatalf("deleting should release storage, used = %d", srv.used)
	}
	if rec := rawUpload(t, handler, map[string][]byte{"b.bin": bytes.Repeat([]byte("b"), 100)}); rec.Code != http.StatusCreated {
		t.Fatalf("upload after freeing space status = %d", rec.Code)
	}
}

func TestExpiredItemsReleaseStorage(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxTotalSize: 100, TTL: time.Minute})
	clock := &testClock{now: time.Now()}
	srv.now = clock.Now
	handler := srv.Routes()

	uploadFiles(t, handler, map[string][]byte{"a.bin": bytes.Repeat([]byte("a"), 100)})
	clock.Advance(2 * time.Minute)
	srv.cleanupExpired()
	if srv.used != 0 {
		t.Fatalf("expiry should release storage, used = %d", srv.used)
	}
}

func TestItemCountLimit(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxItems: 2})
	handler := srv.Routes()
	createText(t, handler, "one", "1")
	createText(t, handler, "two", "2")

	req := httptest.NewRequest(http.MethodPost, "/api/items/text", strings.NewReader(`{"text":"3"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("third item status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTextAndPasswordSizeLimits(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxTextSize: 10})
	handler := srv.Routes()
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/items/text", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post(`{"text":"0123456789"}`); got != http.StatusCreated {
		t.Fatalf("text at the limit status = %d", got)
	}
	if got := post(`{"text":"01234567890"}`); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized text status = %d", got)
	}
	if got := post(`{"text":"` + strings.Repeat("x", 1<<20) + `"}`); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body status = %d", got)
	}
	if got := post(`{"text":"hi","password":"` + strings.Repeat("p", maxPasswordLength+1) + `"}`); got != http.StatusBadRequest {
		t.Fatalf("overlong password status = %d", got)
	}
}

func TestConfigEndpointExposesLimits(t *testing.T) {
	srv := newLimitedServer(t, Config{MaxFileSize: 1234})
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config status = %d", rec.Code)
	}
	var cfg struct {
		MaxFileSize int64 `json:"maxFileSize"`
		MaxItems    int   `json:"maxItems"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.MaxFileSize != 1234 || cfg.MaxItems != defaultMaxItems {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func unlockFrom(handler http.Handler, remoteAddr, id, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/items/"+id+"/unlock", strings.NewReader(`{"password":`+quote(password)+`}`))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestUnlockLocksOutRepeatedWrongPasswords(t *testing.T) {
	srv, clock := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createTextWithOptions(t, handler, "secret.txt", "classified", 0, "correct horse")

	for i := 0; i < 5; i++ {
		if rec := unlockFrom(handler, "10.0.0.5:1111", created.ID, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i+1, rec.Code)
		}
	}
	rec := unlockFrom(handler, "10.0.0.5:2222", created.ID, "correct horse")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out client status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("lockout should say when to retry")
	}

	// Another machine is not affected by the lockout.
	if rec := unlockFrom(handler, "10.0.0.6:3333", created.ID, "correct horse"); rec.Code != http.StatusOK {
		t.Fatalf("other client status = %d, body = %s", rec.Code, rec.Body.String())
	}

	clock.Advance(2 * time.Minute)
	if rec := unlockFrom(handler, "10.0.0.5:4444", created.ID, "correct horse"); rec.Code != http.StatusOK {
		t.Fatalf("status after lockout expired = %d", rec.Code)
	}
}

func TestSuccessfulUnlockResetsFailureCount(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createTextWithOptions(t, handler, "secret.txt", "classified", 0, "pw-123")

	for round := 0; round < 3; round++ {
		for i := 0; i < 4; i++ {
			unlockFrom(handler, "10.0.0.5:1", created.ID, "wrong")
		}
		if rec := unlockFrom(handler, "10.0.0.5:1", created.ID, "pw-123"); rec.Code != http.StatusOK {
			t.Fatalf("round %d unlock status = %d", round, rec.Code)
		}
	}
}

func TestUnlockLockoutPerItemAcrossClients(t *testing.T) {
	srv, _ := newTestServer(t, 5*time.Minute)
	handler := srv.Routes()
	created := createTextWithOptions(t, handler, "secret.txt", "classified", 0, "pw-123")

	for i := 0; i < 60; i++ {
		unlockFrom(handler, fmt.Sprintf("10.0.%d.%d:1", i/200, i%200), created.ID, "wrong")
	}
	if rec := unlockFrom(handler, "10.9.9.9:1", created.ID, "pw-123"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("distributed guessing should lock the item, status = %d", rec.Code)
	}
}

func TestPasswordIsStoredAsSlowSaltedHash(t *testing.T) {
	salt1, hash1, err := newPasswordDigest("hunter22")
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	salt2, hash2, _ := newPasswordDigest("hunter22")
	if bytes.Equal(salt1, salt2) || bytes.Equal(hash1, hash2) {
		t.Fatalf("equal passwords must not produce equal digests")
	}
	if !passwordMatches(salt1, hash1, "  hunter22 ") || passwordMatches(salt1, hash1, "hunter23") {
		t.Fatalf("password verification is wrong")
	}
}
