package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Unit tests for session tokens. They live in package server (unlike the
// HTTP tests in server_test) to control the clock and secret directly.

func testServer(secret string, now time.Time) *Server {
	return &Server{
		apiKeyHash:    HashAPIKey("the-key"),
		sessionSecret: []byte(secret),
		now:           func() time.Time { return now },
	}
}

func TestSessionToken(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := testServer("secret-one-secret-one-secret-one", now)
	tok := s.newSession(now.Add(time.Hour))

	if !s.validSession(tok) {
		t.Fatalf("fresh token %q rejected", tok)
	}

	// Expired: the same token an hour later.
	later := testServer("secret-one-secret-one-secret-one", now.Add(time.Hour))
	if later.validSession(tok) {
		t.Error("token accepted at its expiry time")
	}

	// Signed with another secret (e.g. after rotation).
	rotated := testServer("secret-two-secret-two-secret-two", now)
	if rotated.validSession(tok) {
		t.Error("token accepted under a different secret")
	}

	// Extending the expiry invalidates the signature.
	parts := strings.Split(tok, ".")
	extended := parts[0] + "." + "9" + parts[1] + "." + parts[2]
	if s.validSession(extended) {
		t.Error("token with edited expiry accepted")
	}

	for _, bad := range []string{"", "v1", "v1.123", "v2." + parts[1] + "." + parts[2], "v1.abc." + parts[2], tok + "x"} {
		if s.validSession(bad) {
			t.Errorf("malformed token %q accepted", bad)
		}
	}
}

// Owner cookies issued before the signing code was generalized must keep
// working: v1.<exp>.<HMAC(purpose + exp)>.
func TestOwnerTokenFormatUnchanged(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := testServer("secret-one-secret-one-secret-one", now)
	exp := now.Add(time.Hour)
	e := strconv.FormatInt(exp.Unix(), 10)
	m := hmac.New(sha256.New, []byte("secret-one-secret-one-secret-one"))
	m.Write([]byte("photosync owner session v1\n" + e))
	legacy := "v1." + e + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if got := s.newSession(exp); got != legacy {
		t.Errorf("owner token = %q, want legacy format %q", got, legacy)
	}
}

func TestReviewerToken(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := testServer("secret-one-secret-one-secret-one", now)
	tok := s.newReviewerSession(now.Add(time.Hour), "SID123", "RID456")

	sid, rid, ok := s.reviewerSession(tok)
	if !ok || sid != "SID123" || rid != "RID456" {
		t.Fatalf("reviewerSession = %q %q %v", sid, rid, ok)
	}

	// Swapping in another reviewer's ID breaks the signature.
	if _, _, ok := s.reviewerSession(strings.Replace(tok, "RID456", "RID999", 1)); ok {
		t.Error("token with edited reviewer accepted")
	}
	// The two cookie kinds aren't interchangeable.
	owner := s.newSession(now.Add(time.Hour))
	if _, _, ok := s.reviewerSession(owner); ok {
		t.Error("owner token accepted as reviewer token")
	}
	if s.validSession(tok) {
		t.Error("reviewer token accepted as owner token")
	}
	// Same shape, wrong purpose: an owner-purpose MAC over reviewer fields.
	forged := s.sign(ownerPurpose, now.Add(time.Hour), "SID123", "RID456")
	if _, _, ok := s.reviewerSession(forged); ok {
		t.Error("reviewer token signed for the owner purpose accepted")
	}
	// Expired.
	later := testServer("secret-one-secret-one-secret-one", now.Add(time.Hour))
	if _, _, ok := later.reviewerSession(tok); ok {
		t.Error("expired reviewer token accepted")
	}
}

func TestValidKey(t *testing.T) {
	s := testServer("secret-one-secret-one-secret-one", time.Now())
	if !s.validKey("the-key") {
		t.Error("correct key rejected")
	}
	for _, k := range []string{"", "the-key ", "THE-KEY", "the-ke"} {
		if s.validKey(k) {
			t.Errorf("key %q accepted", k)
		}
	}
}
