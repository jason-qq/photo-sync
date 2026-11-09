package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"photo-sync/internal/model"
	"photo-sync/internal/store"
)

// fakeBlobs is an in-memory blob.Store. Presigned URLs are "put://<key>".
type fakeBlobs struct {
	objects    map[string]bool
	presignErr error
	existsErr  error
}

func (f *fakeBlobs) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	if f.presignErr != nil {
		return "", f.presignErr
	}
	return "put://" + key, nil
}

func (f *fakeBlobs) Exists(_ context.Context, key string) (bool, error) {
	return f.objects[key], f.existsErr
}

var fixedNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.FixedZone("PST", -8*3600))

func newService(t *testing.T) (*Service, *fakeBlobs) {
	t.Helper()
	blobs := &fakeBlobs{objects: map[string]bool{}}
	svc, err := New(Config{Store: store.NewMemory(), Blobs: blobs, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return svc, blobs
}

func mustSession(t *testing.T, svc *Service) model.Session {
	t.Helper()
	sess, err := svc.CreateSession(context.Background(), "Shoot", "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New with no deps succeeded")
	}
}

func TestCreateSession(t *testing.T) {
	svc, _ := newService(t)
	sess, err := svc.CreateSession(context.Background(), "  Studio A  ", "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" || sess.Title != "Studio A" || sess.Date != "2026-11-09" {
		t.Errorf("unexpected session: %+v", sess)
	}
	if !sess.CreatedAt.Equal(fixedNow) || sess.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v in UTC", sess.CreatedAt, fixedNow)
	}
}

func TestRegisterPhoto(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	sid := mustSession(t, svc).ID

	reg, err := svc.RegisterPhoto(ctx, sid, "DSC0001.ARW", hashA, false)
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Photo
	if !reg.Created || p.Status != model.StatusPending || p.SessionID != sid || p.Filename != "DSC0001.ARW" {
		t.Fatalf("unexpected registration: %+v", reg)
	}
	if reg.Uploads == nil || reg.Uploads.Thumb != "put://"+p.ThumbKey() || reg.Uploads.Preview != "put://"+p.PreviewKey() {
		t.Fatalf("unexpected uploads: %+v", reg.Uploads)
	}

	t.Run("duplicate", func(t *testing.T) {
		dup, err := svc.RegisterPhoto(ctx, sid, "renamed.ARW", hashA, false)
		if err != nil {
			t.Fatal(err)
		}
		if dup.Created || dup.Photo.ID != p.ID || dup.Uploads != nil {
			t.Errorf("unexpected: %+v", dup)
		}
	})

	t.Run("duplicate forced", func(t *testing.T) {
		forced, err := svc.RegisterPhoto(ctx, sid, "DSC0001.ARW", hashA, true)
		if err != nil {
			t.Fatal(err)
		}
		if forced.Created || forced.Photo.ID != p.ID || forced.Uploads == nil {
			t.Errorf("unexpected: %+v", forced)
		}
	})
}

func TestRegisterPhotoErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown session", func(t *testing.T) {
		svc, _ := newService(t)
		if _, err := svc.RegisterPhoto(ctx, "nope", "a.ARW", hashA, false); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("presign failure", func(t *testing.T) {
		svc, blobs := newService(t)
		sid := mustSession(t, svc).ID
		boom := errors.New("boom")
		blobs.presignErr = boom
		if _, err := svc.RegisterPhoto(ctx, sid, "a.ARW", hashA, false); !errors.Is(err, boom) {
			t.Errorf("err = %v, want wrapped boom", err)
		}
	})
}

func TestCompletePhoto(t *testing.T) {
	ctx := context.Background()
	svc, blobs := newService(t)
	sid := mustSession(t, svc).ID
	reg, _ := svc.RegisterPhoto(ctx, sid, "a.ARW", hashA, false)
	p := reg.Photo

	_, err := svc.CompletePhoto(ctx, sid, p.ID)
	if !errors.Is(err, ErrNotUploaded) || err.Error() != "thumb has not been uploaded" {
		t.Fatalf("nothing uploaded: err = %v", err)
	}

	blobs.objects[p.ThumbKey()] = true
	_, err = svc.CompletePhoto(ctx, sid, p.ID)
	if !errors.Is(err, ErrNotUploaded) || err.Error() != "preview has not been uploaded" {
		t.Fatalf("thumb only: err = %v", err)
	}

	blobs.objects[p.PreviewKey()] = true
	for range 2 { // idempotent
		done, err := svc.CompletePhoto(ctx, sid, p.ID)
		if err != nil || done.Status != model.StatusReady {
			t.Fatalf("complete: %+v, %v", done, err)
		}
	}

	// Persisted: a duplicate registration now reports ready.
	dup, _ := svc.RegisterPhoto(ctx, sid, "a.ARW", hashA, false)
	if dup.Photo.Status != model.StatusReady {
		t.Errorf("stored status = %s, want ready", dup.Photo.Status)
	}
}

func TestCompletePhotoErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown photo", func(t *testing.T) {
		svc, _ := newService(t)
		sid := mustSession(t, svc).ID
		if _, err := svc.CompletePhoto(ctx, sid, "nope"); !errors.Is(err, ErrPhotoNotFound) {
			t.Errorf("err = %v, want ErrPhotoNotFound", err)
		}
	})

	t.Run("blob check failure", func(t *testing.T) {
		svc, blobs := newService(t)
		sid := mustSession(t, svc).ID
		reg, _ := svc.RegisterPhoto(ctx, sid, "a.ARW", hashA, false)
		boom := errors.New("boom")
		blobs.existsErr = boom
		_, err := svc.CompletePhoto(ctx, sid, reg.Photo.ID)
		if !errors.Is(err, boom) || errors.Is(err, ErrNotUploaded) {
			t.Errorf("err = %v, want wrapped boom", err)
		}
	})
}
