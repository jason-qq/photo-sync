package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result()
}

func TestServesApp(t *testing.T) {
	h := Handler(Config{ImageOrigins: []string{"https://images.example"}})

	tests := []struct {
		path, contentType, contains string
	}{
		{"/", "text/html", `<script src="app.js"`},
		{"/app.js", "text/javascript", "function route"},
		{"/app.css", "text/css", ".grid"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp := get(t, h, http.MethodGet, tt.path)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tt.contentType) {
				t.Errorf("Content-Type = %q, want %s", ct, tt.contentType)
			}
			if !strings.Contains(string(body), tt.contains) {
				t.Errorf("body missing %q", tt.contains)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "img-src 'self' https://images.example") || !strings.Contains(csp, "default-src 'self'") {
				t.Errorf("CSP = %q", csp)
			}
			if resp.Header.Get("Referrer-Policy") != "no-referrer" {
				t.Error("missing Referrer-Policy")
			}
		})
	}
}

func TestRejectsOtherMethods(t *testing.T) {
	resp := get(t, Handler(Config{}), http.MethodPost, "/")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", resp.StatusCode)
	}
}

func TestNotFound(t *testing.T) {
	resp := get(t, Handler(Config{}), http.MethodGet, "/nope.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope.js = %d, want 404", resp.StatusCode)
	}
}
