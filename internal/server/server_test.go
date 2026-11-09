package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"photo-sync/internal/api"
	"photo-sync/internal/blob"
	"photo-sync/internal/store"
)

const testKey = "test-api-key"

// env runs the API and a Local blob store behind one test server, like
// cmd/api does in local mode.
type env struct {
	t  *testing.T
	ts *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	blobs := blob.NewLocal(t.TempDir(), ts.URL, []byte("secret"))
	srv, err := New(Config{Store: store.NewMemory(), Blobs: blobs, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", srv)
	mux.Handle("PUT "+blob.LocalPathPrefix+"{key...}", blobs)
	return &env{t, ts}
}

// call sends an authenticated JSON request and decodes the JSON response
// into out (if non-nil). It returns the status code.
func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	return e.do(method, path, testKey, body, out)
}

func (e *env) do(method, path, key string, body any, out any) int {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (e *env) upload(url string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("jpeg"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("upload: status %d", resp.StatusCode)
	}
}

func (e *env) createSession() string {
	e.t.Helper()
	var s struct{ ID string }
	if code := e.call("POST", "/api/sessions", map[string]string{"title": "Shoot", "date": "2026-11-09"}, &s); code != http.StatusCreated {
		e.t.Fatalf("create session: %d", code)
	}
	return s.ID
}

var hashA = strings.Repeat("a", 64)

func TestUploadFlow(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := "/api/sessions/" + sid + "/photos"

	// New photo: 201 with upload URLs.
	var p api.PhotoResponse
	if code := e.call("POST", photos, map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &p); code != http.StatusCreated {
		t.Fatalf("create photo: %d", code)
	}
	if p.PhotoID == "" || p.Status != "pending" || p.UploadURLs == nil {
		t.Fatalf("unexpected response: %+v", p)
	}
	complete := photos + "/" + p.PhotoID + "/complete"

	// Complete before uploading: 409.
	if code := e.call("POST", complete, nil, nil); code != http.StatusConflict {
		t.Fatalf("complete before upload: %d, want 409", code)
	}
	e.upload(p.UploadURLs.Thumb)
	if code := e.call("POST", complete, nil, nil); code != http.StatusConflict {
		t.Fatalf("complete with only thumb: %d, want 409", code)
	}
	e.upload(p.UploadURLs.Preview)

	// Complete: 200 ready, and idempotent.
	for range 2 {
		var done api.PhotoResponse
		if code := e.call("POST", complete, nil, &done); code != http.StatusOK || done.Status != "ready" {
			t.Fatalf("complete: %d %+v", code, done)
		}
	}

	// Same hash again: 200, same photo, no upload URLs.
	var dup api.PhotoResponse
	if code := e.call("POST", photos, map[string]string{"filename": "renamed.ARW", "hash": hashA}, &dup); code != http.StatusOK {
		t.Fatalf("duplicate: %d, want 200", code)
	}
	if dup.PhotoID != p.PhotoID || dup.Status != "ready" || dup.UploadURLs != nil {
		t.Fatalf("duplicate response: %+v", dup)
	}

	// force=true: 200, same photo, fresh upload URLs.
	var forced api.PhotoResponse
	if code := e.call("POST", photos+"?force=true", map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &forced); code != http.StatusOK {
		t.Fatalf("force: %d, want 200", code)
	}
	if forced.PhotoID != p.PhotoID || forced.UploadURLs == nil {
		t.Fatalf("force response: %+v", forced)
	}

	// Same hash in another session: a new photo.
	var other api.PhotoResponse
	otherPhotos := "/api/sessions/" + e.createSession() + "/photos"
	if code := e.call("POST", otherPhotos, map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &other); code != http.StatusCreated {
		t.Fatalf("other session: %d, want 201", code)
	}
	if other.PhotoID == p.PhotoID {
		t.Fatal("other session reused photo ID")
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	body := map[string]string{"title": "x", "date": "2026-11-09"}
	for _, key := range []string{"", "wrong-key"} {
		if code := e.do("POST", "/api/sessions", key, body, nil); code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", key, code)
		}
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := "/api/sessions/" + sid + "/photos"

	tests := []struct {
		name string
		path string
		body any
		want int
	}{
		{"session missing title", "/api/sessions", map[string]string{"date": "2026-11-09"}, 400},
		{"session blank title", "/api/sessions", map[string]string{"title": "  ", "date": "2026-11-09"}, 400},
		{"session bad date", "/api/sessions", map[string]string{"title": "x", "date": "09/11/2026"}, 400},
		{"unknown field", "/api/sessions", map[string]string{"title": "x", "date": "2026-11-09", "extra": "1"}, 400},
		{"malformed JSON", "/api/sessions", `{"title":`, 400},
		{"trailing data", "/api/sessions", `{"title":"x","date":"2026-11-09"}{}`, 400},
		{"photo bad hash", photos, map[string]string{"filename": "a.ARW", "hash": "abc"}, 400},
		{"photo uppercase hash", photos, map[string]string{"filename": "a.ARW", "hash": strings.Repeat("A", 64)}, 400},
		{"photo path in filename", photos, map[string]string{"filename": `dir\a.ARW`, "hash": hashA}, 400},
		{"photo missing filename", photos, map[string]string{"hash": hashA}, 400},
		{"photo unknown session", "/api/sessions/nope/photos", map[string]string{"filename": "a.ARW", "hash": hashA}, 404},
		{"complete unknown photo", photos + "/nope/complete", nil, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp struct{ Error string }
			if code := e.call("POST", tt.path, tt.body, &resp); code != tt.want {
				t.Errorf("status %d, want %d (error %q)", code, tt.want, resp.Error)
			}
			if resp.Error == "" {
				t.Error("missing error message")
			}
		})
	}
}
