package blob

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testContract runs the behaviour every Store must share, end to end over
// HTTP: presigned URLs are used exactly as the uploader and browser use
// them. newStore must return an empty store; it is called once per subtest.
func testContract(t *testing.T, newStore func(t *testing.T) Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s Store)
	}{
		{"RoundTrip", testRoundTrip},
		{"Missing", testMissing},
		{"Overwrite", testOverwrite},
		{"MethodBound", testMethodBound},
		{"KeyBound", testKeyBound},
		{"Tampered", testTampered},
		{"Expired", testExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newStore(t)) })
	}
}

var ctx = context.Background()

const key = "sessions/s1/p1/preview.jpg"

// put uploads body like the uploader does (Content-Type is part of an S3
// upload URL's signature) and returns the status code.
func put(t *testing.T, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func presignPut(t *testing.T, s Store, key string, ttl time.Duration) string {
	t.Helper()
	url, err := s.PresignPut(ctx, key, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func presignGet(t *testing.T, s Store, key string, ttl time.Duration) string {
	t.Helper()
	url, err := s.PresignGet(ctx, key, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func mustPut(t *testing.T, s Store, key, body string) {
	t.Helper()
	if code := put(t, presignPut(t, s, key, time.Minute), body); code != http.StatusOK {
		t.Fatalf("PUT %s = %d", key, code)
	}
}

func testRoundTrip(t *testing.T, s Store) {
	mustPut(t, s, key, "jpegbytes")
	if ok, err := s.Exists(ctx, key); !ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	code, body, h := get(t, presignGet(t, s, key, time.Minute))
	if code != http.StatusOK || body != "jpegbytes" {
		t.Fatalf("GET = %d %q", code, body)
	}
	if ct := h.Get("Content-Type"); ct != ContentType {
		t.Errorf("Content-Type = %q, want %s", ct, ContentType)
	}
}

func testMissing(t *testing.T, s Store) {
	if ok, err := s.Exists(ctx, key); ok || err != nil {
		t.Errorf("Exists before upload = %v, %v", ok, err)
	}
	if code, _, _ := get(t, presignGet(t, s, key, time.Minute)); code != http.StatusNotFound {
		t.Errorf("GET before upload = %d, want 404", code)
	}
}

// Re-uploading (uploader -force) replaces the object.
func testOverwrite(t *testing.T, s Store) {
	mustPut(t, s, key, "first")
	mustPut(t, s, key, "second")
	if _, body, _ := get(t, presignGet(t, s, key, time.Minute)); body != "second" {
		t.Errorf("after overwrite: %q", body)
	}
}

// An upload URL can't read and a view URL can't write.
func testMethodBound(t *testing.T, s Store) {
	mustPut(t, s, key, "original")
	putURL := presignPut(t, s, key, time.Minute)
	getURL := presignGet(t, s, key, time.Minute)

	if code, _, _ := get(t, putURL); code != http.StatusForbidden {
		t.Errorf("GET with upload URL = %d, want 403", code)
	}
	if code := put(t, getURL, "overwrite"); code != http.StatusForbidden {
		t.Errorf("PUT with view URL = %d, want 403", code)
	}
	if _, body, _ := get(t, getURL); body != "original" {
		t.Errorf("object changed to %q", body)
	}
}

// A URL for one key can't be redirected to another.
func testKeyBound(t *testing.T, s Store) {
	other := strings.Replace(presignPut(t, s, key, time.Minute), "/p1/", "/p2/", 1)
	if code := put(t, other, "x"); code != http.StatusForbidden {
		t.Errorf("PUT to other key = %d, want 403", code)
	}
	if ok, _ := s.Exists(ctx, "sessions/s1/p2/preview.jpg"); ok {
		t.Error("rejected upload was stored")
	}
}

func testTampered(t *testing.T, s Store) {
	url := presignPut(t, s, key, time.Minute)
	// Change one hex digit of the signature (it is the last query value
	// for Local and appears as X-Amz-Signature for S3).
	i := strings.LastIndex(url, "ig")
	if i < 0 {
		t.Fatalf("no signature in %s", url)
	}
	eq := i + strings.IndexByte(url[i:], '=') + 1
	flip := byte('0')
	if url[eq] == '0' {
		flip = '1'
	}
	tampered := url[:eq] + string(flip) + url[eq+1:]
	if code := put(t, tampered, "x"); code != http.StatusForbidden {
		t.Errorf("PUT with tampered signature = %d, want 403", code)
	}
}

func testExpired(t *testing.T, s Store) {
	mustPut(t, s, key, "x")
	putURL := presignPut(t, s, key, time.Second)
	getURL := presignGet(t, s, key, time.Second)
	time.Sleep(2100 * time.Millisecond)
	if code := put(t, putURL, "late"); code != http.StatusForbidden {
		t.Errorf("expired PUT = %d, want 403", code)
	}
	if code, _, _ := get(t, getURL); code != http.StatusForbidden {
		t.Errorf("expired GET = %d, want 403", code)
	}
}
