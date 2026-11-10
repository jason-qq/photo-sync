package server

import (
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
