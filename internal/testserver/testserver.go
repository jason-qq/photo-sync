// Package testserver starts the full local app (API, UI, in-memory store,
// disk-backed blobs) over HTTP for tests, wired exactly as cmd/api wires it.
package testserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"photo-sync/internal/app"
	"photo-sync/internal/blob"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
)

// APIKey is the owner API key the test server accepts.
const APIKey = "test-api-key"

// Env is a running test app.
type Env struct {
	URL string // base URL, e.g. http://127.0.0.1:54321
	// BlobDir is where uploaded renditions are stored.
	BlobDir string
}

// Start runs the app until the test ends.
func Start(t testing.TB) *Env {
	t.Helper()
	// The blob store's URLs must point at the server, so start the server
	// first and install the handler once its URL is known.
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	h, err := app.New(app.Config{
		APIKeyHash:    server.HashAPIKey(APIKey),
		SessionSecret: []byte("test-session-secret-at-least-32-bytes"),
		Store:         store.NewMemory(),
		Blobs:         blob.NewLocal(dir, ts.URL, []byte("secret")),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", h)
	return &Env{URL: ts.URL, BlobDir: dir}
}
