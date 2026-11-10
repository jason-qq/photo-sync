package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"photo-sync/internal/blob"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
)

// remoteBlobs is a blob.Store whose URLs point at another origin, like S3.
type remoteBlobs struct{ base string }

func (r remoteBlobs) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return r.base + "/" + key + "?sig=put", nil
}
func (r remoteBlobs) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return r.base + "/" + key + "?sig=get", nil
}
func (remoteBlobs) Exists(context.Context, string) (bool, error) { return false, nil }

var (
	testHash   = server.HashAPIKey("k")
	testSecret = []byte("app-test-session-secret-32-bytes!")
)

// A form on another site posting to the API (e.g. login CSRF, or riding
// the session cookie) is refused; the same request without browser
// cross-origin headers, as the uploader sends it, is not.
func TestCrossOriginProtection(t *testing.T) {
	h := newApp(t, remoteBlobs{base: "https://bucket.s3.example.com"})
	login := func(set func(*http.Request)) int {
		req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"apiKey":"k"}`))
		set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := login(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", code)
	}
	if code := login(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); code != http.StatusForbidden {
		t.Errorf("POST with foreign Origin = %d, want 403", code)
	}
	if code := login(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }); code != http.StatusNoContent {
		t.Errorf("same-origin POST = %d, want 204", code)
	}
	if code := login(func(*http.Request) {}); code != http.StatusNoContent {
		t.Errorf("non-browser POST = %d, want 204", code)
	}
}

func serve(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result()
}

func newApp(t *testing.T, blobs blob.Store) http.Handler {
	t.Helper()
	h, err := New(Config{APIKeyHash: testHash, SessionSecret: testSecret, Store: store.NewMemory(), Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRoutesWithLocalBlobs(t *testing.T) {
	h := newApp(t, blob.NewLocal(t.TempDir(), "http://localhost:8080", []byte("s")))

	if code := serve(t, h, "GET", "/api/sessions").StatusCode; code != http.StatusUnauthorized {
		t.Errorf("API without key = %d, want 401", code)
	}
	ui := serve(t, h, "GET", "/")
	if ui.StatusCode != http.StatusOK {
		t.Errorf("UI = %d", ui.StatusCode)
	}
	if csp := ui.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' http://localhost:8080;") {
		t.Errorf("CSP = %q, want img-src for the local blob origin", csp)
	}
	// Mounted: an unsigned request reaches the blob handler and is refused
	// there (403), rather than falling through to the UI (404).
	if code := serve(t, h, "GET", blob.LocalPathPrefix+"a.jpg").StatusCode; code != http.StatusForbidden {
		t.Errorf("blob route = %d, want 403 from the blob handler", code)
	}
}

func TestRoutesWithRemoteBlobs(t *testing.T) {
	h := newApp(t, remoteBlobs{base: "https://bucket.s3.example.com"})

	csp := serve(t, h, "GET", "/").Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' https://bucket.s3.example.com;") {
		t.Errorf("CSP = %q, want img-src for the bucket origin", csp)
	}
	if code := serve(t, h, "GET", blob.LocalPathPrefix+"a.jpg").StatusCode; code != http.StatusNotFound {
		t.Errorf("blob route = %d, want 404 (not mounted)", code)
	}
}

func TestNewErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no store", Config{APIKeyHash: testHash, SessionSecret: testSecret, Blobs: remoteBlobs{"https://b"}}},
		{"no blobs", Config{APIKeyHash: testHash, SessionSecret: testSecret, Store: store.NewMemory()}},
		{"no API key", Config{SessionSecret: testSecret, Store: store.NewMemory(), Blobs: remoteBlobs{"https://b"}}},
		{"short session secret", Config{APIKeyHash: testHash, SessionSecret: []byte("short"), Store: store.NewMemory(), Blobs: remoteBlobs{"https://b"}}},
		{"blob URLs without origin", Config{APIKeyHash: testHash, SessionSecret: testSecret, Store: store.NewMemory(), Blobs: remoteBlobs{""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Error("New succeeded")
			}
		})
	}
}
