package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"photo-sync/internal/model"
)

func newSession(t *testing.T, m *Memory) string {
	t.Helper()
	id := model.NewID()
	if err := m.CreateSession(context.Background(), model.Session{ID: id, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func photo(sid, hash string) model.Photo {
	return model.Photo{ID: model.NewID(), SessionID: sid, Hash: hash, Status: model.StatusPending}
}

func TestCreatePhotoDedupe(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	s1, s2 := newSession(t, m), newSession(t, m)

	first, created, err := m.CreatePhoto(ctx, photo(s1, "h1"))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	dup, created, err := m.CreatePhoto(ctx, photo(s1, "h1"))
	if err != nil || created {
		t.Fatalf("duplicate: created=%v err=%v", created, err)
	}
	if dup.ID != first.ID {
		t.Errorf("duplicate returned %s, want existing %s", dup.ID, first.ID)
	}

	// Same hash in a different session is a separate photo.
	other, created, err := m.CreatePhoto(ctx, photo(s2, "h1"))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("other session: created=%v err=%v id=%s", created, err, other.ID)
	}
}

func TestCreatePhotoConcurrentDedupe(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	sid := newSession(t, m)

	const n = 50
	var wg sync.WaitGroup
	ids := make([]string, n)
	createdCount := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, created, err := m.CreatePhoto(ctx, photo(sid, "same"))
			if err != nil {
				t.Error(err)
			}
			ids[i], createdCount[i] = p.ID, created
		}()
	}
	wg.Wait()

	created := 0
	for i := range n {
		if createdCount[i] {
			created++
		}
		if ids[i] != ids[0] {
			t.Fatalf("got differing photo IDs %s and %s", ids[0], ids[i])
		}
	}
	if created != 1 {
		t.Errorf("created %d photos, want 1", created)
	}
}

func TestNotFound(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if _, err := m.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession: %v", err)
	}
	if _, _, err := m.CreatePhoto(ctx, photo("nope", "h")); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreatePhoto: %v", err)
	}
	sid := newSession(t, m)
	if _, err := m.GetPhoto(ctx, sid, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPhoto: %v", err)
	}
	if err := m.SetPhotoStatus(ctx, sid, "nope", model.StatusReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPhotoStatus: %v", err)
	}
}

func TestSetPhotoStatus(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	sid := newSession(t, m)
	p, _, _ := m.CreatePhoto(ctx, photo(sid, "h"))
	if err := m.SetPhotoStatus(ctx, sid, p.ID, model.StatusReady); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetPhoto(ctx, sid, p.ID)
	if got.Status != model.StatusReady {
		t.Errorf("status = %s, want ready", got.Status)
	}
}
