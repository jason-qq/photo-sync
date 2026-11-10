package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Authentication.
//
// Owner: the server never holds the owner API key itself, only its SHA-256
// hash (see HashAPIKey). A presented key is hashed and compared in constant
// time. A plain hash is enough because the key is long and random; slow
// password hashes only matter for human-chosen secrets. The browser
// exchanges the key for a session cookie (POST /api/login), so page scripts
// never hold the key.
//
// Reviewer: the link token is exchanged for a reviewer cookie (POST
// /api/review/session) naming the session and reviewer. The cookie only
// says who the reviewer is; every request re-checks the reviewer record,
// so revoking or expiring the link takes effect immediately.
//
// Both cookies are stateless signed tokens:
//
//	v1.<expiry unix seconds>[.<field>...].<base64url HMAC-SHA256(secret, purpose + payload)>
//
// where payload is the expiry and fields joined with ".". The purpose
// string differs per cookie kind, so one kind can never be replayed as the
// other. Anyone with the secret could mint cookies, so it lives in a secret
// store in production. Rotating the secret logs every browser out.

// Cookie names.
const (
	// SessionCookie is the name of the owner session cookie.
	SessionCookie = "photosync_owner"
	// ReviewerCookie is the name of the reviewer session cookie.
	ReviewerCookie = "photosync_reviewer"
)

// DefaultSessionTTL is how long an owner session lasts.
const DefaultSessionTTL = 7 * 24 * time.Hour

// ReviewerSessionTTL caps a reviewer cookie's life. The link's own expiry is
// enforced on every request, so this only bounds a cookie whose link was
// later extended; it matches the longest a link can be valid.
const ReviewerSessionTTL = 90 * 24 * time.Hour

// MinSessionSecretLen is the minimum length of Config.SessionSecret.
const MinSessionSecretLen = 32

// Purposes bind a MAC to one use, so the secret can't be abused to produce
// MACs meaningful anywhere else, and the cookie kinds aren't
// interchangeable.
const (
	ownerPurpose    = "photosync owner session v1\n"
	reviewerPurpose = "photosync reviewer session v1\n"
)

// HashAPIKey returns the SHA-256 hash of an owner API key, as stored in
// Config.APIKeyHash (and, hex-encoded, in the deployed secret store).
func HashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// validKey reports whether key matches the stored hash.
func (s *Server) validKey(key string) bool {
	return key != "" && subtle.ConstantTimeCompare(HashAPIKey(key), s.apiKeyHash) == 1
}

func (s *Server) mac(purpose, payload string) string {
	m := hmac.New(sha256.New, s.sessionSecret)
	m.Write([]byte(purpose + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// sign returns a token for purpose, valid until exp, carrying fields (which
// must not contain ".").
func (s *Server) sign(purpose string, exp time.Time, fields ...string) string {
	payload := strings.Join(append([]string{strconv.FormatInt(exp.Unix(), 10)}, fields...), ".")
	return "v1." + payload + "." + s.mac(purpose, payload)
}

// verify checks a token made by sign for purpose: well formed, exactly n
// fields, unexpired, correctly signed. It returns the fields.
func (s *Server) verify(purpose, token string, n int) ([]string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != n+3 || parts[0] != "v1" {
		return nil, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || s.now().Unix() >= exp {
		return nil, false
	}
	payload := strings.Join(parts[1:len(parts)-1], ".")
	if !hmac.Equal([]byte(parts[len(parts)-1]), []byte(s.mac(purpose, payload))) {
		return nil, false
	}
	return parts[2 : len(parts)-1], true
}

// newSession returns an owner session cookie value valid until exp.
func (s *Server) newSession(exp time.Time) string { return s.sign(ownerPurpose, exp) }

// validSession reports whether v is a valid owner session cookie value.
func (s *Server) validSession(v string) bool {
	_, ok := s.verify(ownerPurpose, v, 0)
	return ok
}

// newReviewerSession returns a reviewer cookie value naming the session
// and reviewer (both ULIDs, so they contain no ".").
func (s *Server) newReviewerSession(exp time.Time, sessionID, reviewerID string) string {
	return s.sign(reviewerPurpose, exp, sessionID, reviewerID)
}

// reviewerSession returns the session and reviewer a reviewer cookie value
// names, if it is validly signed and unexpired. The caller must still check
// that the reviewer's link is active.
func (s *Server) reviewerSession(v string) (sessionID, reviewerID string, ok bool) {
	f, ok := s.verify(reviewerPurpose, v, 2)
	if !ok {
		return "", "", false
	}
	return f[0], f[1], true
}

// authCookie returns a session cookie to set. A non-positive maxAge deletes
// it. HttpOnly keeps it from page scripts; SameSite=Strict keeps browsers
// from sending it on cross-site requests; Secure keeps it off plain HTTP
// (browsers treat http://localhost as secure, so local development still
// works).
func authCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}
