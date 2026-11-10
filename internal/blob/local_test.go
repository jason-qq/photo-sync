package blob

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLocal returns a Local served by a test HTTP server.
func newLocal(t *testing.T) (*Local, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	l := NewLocal(t.TempDir(), ts.URL, []byte("test-secret"))
	mux.Handle(LocalPathPrefix+"{key...}", l)
	return l, ts
}

func TestLocal(t *testing.T) {
	t.Parallel() // the expiry test sleeps
	testContract(t, func(t *testing.T) Store {
		l, _ := newLocal(t)
		return l
	})
}

// Local-specific checks, beyond the shared contract.

func TestLocalPutRejected(t *testing.T) {
	l, ts := newLocal(t)
	good := presignPut(t, l, "a/b.jpg", time.Minute)
	tests := []struct {
		name string
		url  string
	}{
		{"extended expiry", strings.Replace(good, "exp=", "exp=9", 1)},
		{"no signature", ts.URL + LocalPathPrefix + "a/b.jpg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := put(t, tt.url, "x"); code != http.StatusForbidden {
				t.Errorf("PUT = %d, want 403", code)
			}
		})
	}
}

func TestLocalInvalidKeys(t *testing.T) {
	l, _ := newLocal(t)
	for _, key := range []string{"", "../escape.jpg", "a/../../escape.jpg", "/abs.jpg"} {
		if _, err := l.PresignPut(ctx, key, time.Minute); err == nil {
			t.Errorf("PresignPut(%q) succeeded", key)
		}
		if _, err := l.PresignGet(ctx, key, time.Minute); err == nil {
			t.Errorf("PresignGet(%q) succeeded", key)
		}
		if _, err := l.Exists(ctx, key); err == nil {
			t.Errorf("Exists(%q) succeeded", key)
		}
	}
}

func TestLocalGetDirectoryKey(t *testing.T) {
	l, _ := newLocal(t)
	mustPut(t, l, "a/b.jpg", "x")
	// "a" exists on disk as a directory; it must not be served.
	if code, _, _ := get(t, presignGet(t, l, "a", time.Minute)); code != http.StatusNotFound {
		t.Errorf("GET directory = %d, want 404", code)
	}
}
