package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"photo-sync/internal/store"
)

// clock is a settable time source for expiry tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newClockedService(t *testing.T) (*Service, *clock) {
	t.Helper()
	c := &clock{t: fixedNow}
	svc, err := New(Config{Store: store.NewMemory(), Blobs: &fakeBlobs{objects: map[string]bool{}}, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return svc, c
}

func TestCreateReviewer(t *testing.T) {
	ctx := context.Background()
	svc, _ := newClockedService(t)
	sid := mustSession(t, svc).ID

	link, err := svc.CreateReviewer(ctx, sid, "  山田 花子  ", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r := link.Reviewer
	if r.Name != "山田 花子" || r.SessionID != sid || r.Revoked {
		t.Errorf("reviewer = %+v", r)
	}
	if want := fixedNow.UTC().Add(DefaultReviewerTTL); !r.ExpiresAt.Equal(want) {
		t.Errorf("default expiry = %v, want %v", r.ExpiresAt, want)
	}
	if len(link.Token) != 43 || strings.ContainsAny(link.Token, "+/=") {
		t.Errorf("token %q: want 43 base64url chars", link.Token)
	}
	if r.TokenHash == link.Token || r.TokenHash != hashToken(link.Token) {
		t.Error("stored hash must be the token's SHA-256, never the token")
	}

	other, _ := svc.CreateReviewer(ctx, sid, "B", time.Time{})
	if other.Token == link.Token || other.Reviewer.ID == r.ID {
		t.Error("tokens/IDs not unique")
	}

	explicit := fixedNow.Add(48 * time.Hour)
	link2, err := svc.CreateReviewer(ctx, sid, "C", explicit)
	if err != nil || !link2.Reviewer.ExpiresAt.Equal(explicit) {
		t.Errorf("explicit expiry: %v, %v", link2.Reviewer.ExpiresAt, err)
	}

	rs, err := svc.ListReviewers(ctx, sid)
	if err != nil || len(rs) != 3 || rs[0].ID != r.ID {
		t.Errorf("ListReviewers = %d reviewers, %v", len(rs), err)
	}
}

func TestCreateReviewerErrors(t *testing.T) {
	ctx := context.Background()
	svc, _ := newClockedService(t)
	sid := mustSession(t, svc).ID

	if _, err := svc.CreateReviewer(ctx, "nope", "A", time.Time{}); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := svc.ListReviewers(ctx, "nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("list unknown session: %v", err)
	}
	for name, exp := range map[string]time.Time{
		"past":          fixedNow.Add(-time.Hour),
		"now":           fixedNow,
		"too far ahead": fixedNow.Add(MaxReviewerTTL + time.Hour),
	} {
		if _, err := svc.CreateReviewer(ctx, sid, "A", exp); !errors.Is(err, ErrInvalidExpiry) {
			t.Errorf("%s: %v, want ErrInvalidExpiry", name, err)
		}
	}
}

func TestAuthenticateReviewer(t *testing.T) {
	ctx := context.Background()
	svc, clk := newClockedService(t)
	sid := mustSession(t, svc).ID
	link, _ := svc.CreateReviewer(ctx, sid, "A", fixedNow.Add(time.Hour))

	r, err := svc.AuthenticateReviewer(ctx, link.Token)
	if err != nil || r.ID != link.Reviewer.ID {
		t.Fatalf("valid token: %+v, %v", r, err)
	}
	for _, bad := range []string{"", "x", link.Token + "x", strings.Repeat("a", 200)} {
		if _, err := svc.AuthenticateReviewer(ctx, bad); !errors.Is(err, ErrInvalidLink) {
			t.Errorf("token %q: %v, want ErrInvalidLink", bad, err)
		}
	}

	// Expired: rejected; extending revives it.
	clk.t = fixedNow.Add(time.Hour)
	if _, err := svc.AuthenticateReviewer(ctx, link.Token); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("expired: %v", err)
	}
	if _, err := svc.ExtendReviewer(ctx, sid, link.Reviewer.ID, clk.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateReviewer(ctx, link.Token); err != nil {
		t.Errorf("after extend: %v", err)
	}

	// Revoked: rejected immediately, and can't be extended back.
	if _, err := svc.RevokeReviewer(ctx, sid, link.Reviewer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateReviewer(ctx, link.Token); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("revoked: %v", err)
	}
	if _, err := svc.ExtendReviewer(ctx, sid, link.Reviewer.ID, clk.t.Add(48*time.Hour)); !errors.Is(err, ErrReviewerRevoked) {
		t.Errorf("extend revoked: %v, want ErrReviewerRevoked", err)
	}
}

func TestActiveReviewerAndView(t *testing.T) {
	ctx := context.Background()
	svc, clk := newClockedService(t)
	blobs := svc.blobs.(*fakeBlobs)
	sess := mustSession(t, svc)
	link, _ := svc.CreateReviewer(ctx, sess.ID, "A", fixedNow.Add(time.Hour))
	rid := link.Reviewer.ID

	r, err := svc.ActiveReviewer(ctx, sess.ID, rid)
	if err != nil || r.ID != rid {
		t.Fatalf("active: %+v, %v", r, err)
	}
	if _, err := svc.ActiveReviewer(ctx, sess.ID, "nope"); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("unknown reviewer: %v", err)
	}
	if _, err := svc.ActiveReviewer(ctx, mustSession(t, svc).ID, rid); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("reviewer under another session: %v", err)
	}

	// Only ready photos, in filename order.
	b := readyPhoto(t, svc, blobs, sess.ID, "DSC0002.ARW", strings.Repeat("b", 64))
	svc.RegisterPhoto(ctx, sess.ID, "DSC0003.ARW", strings.Repeat("c", 64), false) // pending
	a := readyPhoto(t, svc, blobs, sess.ID, "DSC0001.ARW", hashA)
	v, err := svc.ReviewerView(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if v.Session.ID != sess.ID || v.Session.Title != "Shoot" {
		t.Errorf("session = %+v", v.Session)
	}
	if len(v.Photos) != 2 || v.Photos[0].Photo.ID != a.ID || v.Photos[1].Photo.ID != b.ID || v.Photos[0].Images == nil {
		t.Errorf("photos = %+v", v.Photos)
	}

	// Expiry and revocation take effect on the next request.
	clk.t = fixedNow.Add(time.Hour)
	if _, err := svc.ActiveReviewer(ctx, sess.ID, rid); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("expired: %v", err)
	}
	svc.ExtendReviewer(ctx, sess.ID, rid, clk.t.Add(time.Hour))
	svc.RevokeReviewer(ctx, sess.ID, rid)
	if _, err := svc.ActiveReviewer(ctx, sess.ID, rid); !errors.Is(err, ErrInvalidLink) {
		t.Errorf("revoked: %v", err)
	}
}

func TestExtendAndRevoke(t *testing.T) {
	ctx := context.Background()
	svc, _ := newClockedService(t)
	sid := mustSession(t, svc).ID
	link, _ := svc.CreateReviewer(ctx, sid, "A", time.Time{})
	rid := link.Reviewer.ID

	newExp := fixedNow.Add(30 * 24 * time.Hour)
	r, err := svc.ExtendReviewer(ctx, sid, rid, newExp)
	if err != nil || !r.ExpiresAt.Equal(newExp) {
		t.Fatalf("extend: %+v, %v", r, err)
	}
	if rs, _ := svc.ListReviewers(ctx, sid); !rs[0].ExpiresAt.Equal(newExp) {
		t.Error("extension not persisted")
	}
	if _, err := svc.ExtendReviewer(ctx, sid, rid, fixedNow.Add(-time.Hour)); !errors.Is(err, ErrInvalidExpiry) {
		t.Errorf("extend into past: %v", err)
	}

	for range 2 { // idempotent
		r, err = svc.RevokeReviewer(ctx, sid, rid)
		if err != nil || !r.Revoked {
			t.Fatalf("revoke: %+v, %v", r, err)
		}
	}

	if _, err := svc.ExtendReviewer(ctx, sid, "nope", newExp); !errors.Is(err, ErrReviewerNotFound) {
		t.Errorf("extend unknown: %v", err)
	}
	if _, err := svc.RevokeReviewer(ctx, sid, "nope"); !errors.Is(err, ErrReviewerNotFound) {
		t.Errorf("revoke unknown: %v", err)
	}
	// A reviewer is only reachable through its own session.
	other := mustSession(t, svc).ID
	if _, err := svc.RevokeReviewer(ctx, other, rid); !errors.Is(err, ErrReviewerNotFound) {
		t.Errorf("revoke via other session: %v", err)
	}
}
