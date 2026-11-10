package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"photo-sync/internal/model"
)

// testContract runs the behaviour every Store implementation must share.
// newStore must return an empty store; it is called once per subtest.
func testContract(t *testing.T, newStore func(t *testing.T) Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s Store)
	}{
		{"SessionRoundTrip", testSessionRoundTrip},
		{"PhotoRoundTrip", testPhotoRoundTrip},
		{"CreatePhotoDedupe", testCreatePhotoDedupe},
		{"CreatePhotoConcurrentDedupe", testCreatePhotoConcurrentDedupe},
		{"NotFound", testNotFound},
		{"Exists", testExists},
		{"ListSessions", testListSessions},
		{"ListPhotos", testListPhotos},
		{"SetPhotoStatus", testSetPhotoStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newStore(t)) })
	}
}

var ctx = context.Background()

// ts is a timestamp with sub-second precision, to check it round-trips.
var ts = time.Date(2026, 11, 10, 17, 30, 22, 123456789, time.UTC)

func newSession(t *testing.T, s Store) string {
	t.Helper()
	id := model.NewID()
	if err := s.CreateSession(ctx, model.Session{ID: id, Title: "t", Date: "2026-11-10", CreatedAt: ts}); err != nil {
		t.Fatal(err)
	}
	return id
}

func photo(sid, hash string) model.Photo {
	return model.Photo{
		ID: model.NewID(), SessionID: sid, Filename: "DSC0001.ARW", Hash: hash,
		Status: model.StatusPending, CreatedAt: ts,
	}
}

func mustCreatePhoto(t *testing.T, s Store, p model.Photo) model.Photo {
	t.Helper()
	got, created, err := s.CreatePhoto(ctx, p)
	if err != nil || !created {
		t.Fatalf("create photo: created=%v err=%v", created, err)
	}
	return got
}

func testSessionRoundTrip(t *testing.T, s Store) {
	want := model.Session{ID: model.NewID(), Title: "Studio — Ana", Date: "2026-11-10", CreatedAt: ts}
	if err := s.CreateSession(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Date != want.Date || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func testPhotoRoundTrip(t *testing.T, s Store) {
	sid := newSession(t, s)
	want := photo(sid, "h1")
	mustCreatePhoto(t, s, want)
	got, err := s.GetPhoto(ctx, sid, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.SessionID != want.SessionID || got.Filename != want.Filename ||
		got.Hash != want.Hash || got.Status != want.Status || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func testCreatePhotoDedupe(t *testing.T, s Store) {
	s1, s2 := newSession(t, s), newSession(t, s)

	first := mustCreatePhoto(t, s, photo(s1, "h1"))

	dup, created, err := s.CreatePhoto(ctx, photo(s1, "h1"))
	if err != nil || created {
		t.Fatalf("duplicate: created=%v err=%v", created, err)
	}
	if dup.ID != first.ID {
		t.Errorf("duplicate returned %s, want existing %s", dup.ID, first.ID)
	}

	// Same hash in a different session is a separate photo.
	other, created, err := s.CreatePhoto(ctx, photo(s2, "h1"))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("other session: created=%v err=%v id=%s", created, err, other.ID)
	}
}

func testCreatePhotoConcurrentDedupe(t *testing.T, s Store) {
	sid := newSession(t, s)

	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	createdBy := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, created, err := s.CreatePhoto(ctx, photo(sid, "same"))
			if err != nil {
				t.Error(err)
			}
			ids[i], createdBy[i] = p.ID, created
		}()
	}
	wg.Wait()

	created := 0
	for i := range n {
		if createdBy[i] {
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

func testNotFound(t *testing.T, s Store) {
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession: %v", err)
	}
	if _, _, err := s.CreatePhoto(ctx, photo("nope", "h")); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreatePhoto: %v", err)
	}
	if _, err := s.ListPhotos(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListPhotos: %v", err)
	}
	sid := newSession(t, s)
	if _, err := s.GetPhoto(ctx, sid, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPhoto: %v", err)
	}
	if err := s.SetPhotoStatus(ctx, sid, "nope", model.StatusReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPhotoStatus: %v", err)
	}
}

func testExists(t *testing.T, s Store) {
	sid := newSession(t, s)
	if err := s.CreateSession(ctx, model.Session{ID: sid, Title: "again"}); !errors.Is(err, ErrExists) {
		t.Errorf("CreateSession with taken ID: %v, want ErrExists", err)
	}
	p := mustCreatePhoto(t, s, photo(sid, "h1"))
	reused := photo(sid, "h2")
	reused.ID = p.ID
	if _, _, err := s.CreatePhoto(ctx, reused); !errors.Is(err, ErrExists) {
		t.Errorf("CreatePhoto with taken ID: %v, want ErrExists", err)
	}
	// The failed create must not have claimed its hash.
	if _, created, err := s.CreatePhoto(ctx, photo(sid, "h2")); err != nil || !created {
		t.Errorf("hash h2 after failed create: created=%v err=%v", created, err)
	}
}

func testListSessions(t *testing.T, s Store) {
	got, err := s.ListSessions(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty store: %v, %v (want non-nil empty slice)", got, err)
	}

	want := []string{newSession(t, s), newSession(t, s), newSession(t, s)}
	got, err = s.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sess := range got {
		ids = append(ids, sess.ID)
	}
	if !slices.Equal(ids, want) {
		t.Errorf("IDs = %v, want %v (creation order)", ids, want)
	}
	if got[0].Title != "t" || !got[0].CreatedAt.Equal(ts) {
		t.Errorf("listed session fields: %+v", got[0])
	}
}

func testListPhotos(t *testing.T, s Store) {
	sid, other := newSession(t, s), newSession(t, s)

	got, err := s.ListPhotos(ctx, sid)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty session: %v, %v (want non-nil empty slice)", got, err)
	}

	// Created out of order; includes a duplicate filename (different
	// content) and names with characters sorting around the '#' separator.
	filenames := []string{"DSC0003.ARW", "DSC0001.ARW", "DSC0002.ARW", "DSC0001.ARW", "DSC 0001.ARW", "DSC0001.ARW1"}
	byName := map[string][]string{}
	for i, name := range filenames {
		p := photo(sid, string(rune('a'+i)))
		p.Filename = name
		mustCreatePhoto(t, s, p)
		byName[name] = append(byName[name], p.ID)
	}
	mustCreatePhoto(t, s, photo(other, "a"))

	got, err = s.ListPhotos(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Filename)
	}
	wantNames := []string{"DSC 0001.ARW", "DSC0001.ARW", "DSC0001.ARW", "DSC0001.ARW1", "DSC0002.ARW", "DSC0003.ARW"}
	if !slices.Equal(names, wantNames) {
		t.Errorf("filenames = %q, want %q (this session only, filename order)", names, wantNames)
	}
	// Duplicate filenames are ordered by ID.
	dups := slices.Clone(byName["DSC0001.ARW"])
	slices.Sort(dups)
	if len(got) == len(wantNames) && (got[1].ID != dups[0] || got[2].ID != dups[1]) {
		t.Errorf("duplicate filenames not in ID order")
	}
}

func testSetPhotoStatus(t *testing.T, s Store) {
	sid := newSession(t, s)
	p := mustCreatePhoto(t, s, photo(sid, "h"))
	if err := s.SetPhotoStatus(ctx, sid, p.ID, model.StatusReady); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPhoto(ctx, sid, p.ID)
	if got.Status != model.StatusReady {
		t.Errorf("status = %s, want ready", got.Status)
	}
	// The change is visible through the listing too.
	list, _ := s.ListPhotos(ctx, sid)
	if len(list) != 1 || list[0].Status != model.StatusReady {
		t.Errorf("listed status: %+v", list)
	}
}
