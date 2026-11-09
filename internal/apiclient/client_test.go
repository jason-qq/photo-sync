package apiclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"photo-sync/internal/api"
	"photo-sync/internal/testserver"
)

var hashA = strings.Repeat("a", 64)

func TestUploadFlow(t *testing.T) {
	ctx := context.Background()
	env := testserver.Start(t)
	c := New(env.URL+"/", testserver.APIKey) // trailing slash is tolerated

	sess, err := c.CreateSession(ctx, api.CreateSessionRequest{Title: "Shoot", Date: "2026-11-09"})
	if err != nil {
		t.Fatal(err)
	}

	p, created, err := c.RegisterPhoto(ctx, sess.ID, api.CreatePhotoRequest{Filename: "a.ARW", Hash: hashA}, false)
	if err != nil || !created || p.UploadURLs == nil {
		t.Fatalf("register: %+v created=%v err=%v", p, created, err)
	}
	if err := c.Upload(ctx, p.UploadURLs.Thumb, []byte("thumb")); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, p.UploadURLs.Preview, []byte("preview")); err != nil {
		t.Fatal(err)
	}
	done, err := c.CompletePhoto(ctx, sess.ID, p.PhotoID)
	if err != nil || done.Status != api.PhotoReady {
		t.Fatalf("complete: %+v err=%v", done, err)
	}

	dup, created, err := c.RegisterPhoto(ctx, sess.ID, api.CreatePhotoRequest{Filename: "a.ARW", Hash: hashA}, false)
	if err != nil || created || dup.PhotoID != p.PhotoID || dup.UploadURLs != nil {
		t.Fatalf("duplicate: %+v created=%v err=%v", dup, created, err)
	}
}

func TestAPIErrors(t *testing.T) {
	ctx := context.Background()
	env := testserver.Start(t)

	_, err := New(env.URL, "wrong").CreateSession(ctx, api.CreateSessionRequest{Title: "x", Date: "2026-11-09"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: err = %v, want 401 APIError", err)
	}

	c := New(env.URL, testserver.APIKey)
	_, _, err = c.RegisterPhoto(ctx, "nope", api.CreatePhotoRequest{Filename: "a.ARW", Hash: hashA}, false)
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound || ae.Message != "session not found" {
		t.Errorf("unknown session: err = %v", err)
	}

	sess, _ := c.CreateSession(ctx, api.CreateSessionRequest{Title: "x", Date: "2026-11-09"})
	_, _, err = c.RegisterPhoto(ctx, sess.ID, api.CreatePhotoRequest{Filename: "a.ARW", Hash: "bad"}, false)
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusBadRequest || !strings.HasPrefix(ae.Message, "hash:") {
		t.Errorf("bad hash: err = %v", err)
	}
}

func TestUploadErrorsHideSignature(t *testing.T) {
	ctx := context.Background()
	env := testserver.Start(t)
	c := New(env.URL, testserver.APIKey)

	// Rejected by the blob handler.
	err := c.Upload(ctx, env.URL+"/dev/blob/a/b.jpg?exp=1&sig=SECRETSIG", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Errorf("rejected upload: err = %v", err)
	}

	// Network failure: nothing listening.
	err = c.Upload(ctx, "http://127.0.0.1:1/dev/blob/a/b.jpg?exp=1&sig=SECRETSIG", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Errorf("network failure: err = %v", err)
	}
}
