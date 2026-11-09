package blob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*Local, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	l := NewLocal(t.TempDir(), ts.URL, []byte("test-secret"))
	mux.Handle("PUT "+LocalPathPrefix+"{key...}", l)
	return l, ts
}

func put(t *testing.T, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPresignedPutRoundTrip(t *testing.T) {
	ctx := context.Background()
	l, _ := setup(t)
	key := "sessions/s1/p1/thumb.jpg"

	if ok, _ := l.Exists(ctx, key); ok {
		t.Fatal("object exists before upload")
	}
	url, err := l.PresignPut(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code := put(t, url, "jpegbytes"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if ok, err := l.Exists(ctx, key); !ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	got, _ := os.ReadFile(filepath.Join(l.dir, filepath.FromSlash(key)))
	if string(got) != "jpegbytes" {
		t.Errorf("stored %q", got)
	}
}

func TestPutRejected(t *testing.T) {
	ctx := context.Background()
	l, ts := setup(t)
	good, _ := l.PresignPut(ctx, "a/b.jpg", time.Minute)

	expiredURL, _ := l.PresignPut(ctx, "a/b.jpg", -time.Second)

	// Flip the signature's last hex digit (always to a different value).
	tamper := "0"
	if strings.HasSuffix(good, "0") {
		tamper = "1"
	}

	tests := []struct {
		name string
		url  string
	}{
		{"tampered signature", good[:len(good)-1] + tamper},
		{"different key", strings.Replace(good, "a/b.jpg", "a/c.jpg", 1)},
		{"extended expiry", strings.Replace(good, "exp=", "exp=9", 1)},
		{"expired", expiredURL},
		{"no signature", ts.URL + LocalPathPrefix + "a/b.jpg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := put(t, tt.url, "x"); code != http.StatusForbidden {
				t.Errorf("PUT = %d, want 403", code)
			}
		})
	}
	if ok, _ := l.Exists(ctx, "a/c.jpg"); ok {
		t.Error("rejected upload was stored")
	}
}

func TestInvalidKeys(t *testing.T) {
	ctx := context.Background()
	l, _ := setup(t)
	for _, key := range []string{"", "../escape.jpg", "a/../../escape.jpg", "/abs.jpg"} {
		if _, err := l.PresignPut(ctx, key, time.Minute); err == nil {
			t.Errorf("PresignPut(%q) succeeded", key)
		}
		if _, err := l.Exists(ctx, key); err == nil {
			t.Errorf("Exists(%q) succeeded", key)
		}
	}
}
