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
