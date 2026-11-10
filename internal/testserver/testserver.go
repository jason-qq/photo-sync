// Package testserver starts the full local API stack (server, review service,
// in-memory store, disk-backed blobs) for client-side tests.
package testserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"photo-sync/internal/blob"
	"photo-sync/internal/review"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
)

// APIKey is the owner API key the test server accepts.
const APIKey = "test-api-key"

// Env is a running test API.
type Env struct {
	URL string // base URL, e.g. http://127.0.0.1:54321
	// BlobDir is where uploaded renditions are stored.
	BlobDir string
}

// Start runs the API until the test ends.
func Start(t testing.TB) *Env {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	blobs := blob.NewLocal(dir, ts.URL, []byte("secret"))
	svc, err := review.New(review.Config{Store: store.NewMemory(), Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Service: svc, APIKey: APIKey})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", srv)
	mux.Handle(blob.LocalPathPrefix+"{key...}", blobs)
	return &Env{URL: ts.URL, BlobDir: dir}
}
