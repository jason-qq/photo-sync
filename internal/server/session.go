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

// Owner authentication.
//
// The server never holds the owner API key itself, only its SHA-256 hash
// (see HashAPIKey). A presented key is hashed and compared in constant
// time. A plain hash is enough because the key is long and random; slow
// password hashes only matter for human-chosen secrets.
//
// The browser exchanges the key for a session cookie (POST /api/login), so
// page scripts never hold the key. The cookie is stateless:
//
//	v1.<expiry unix seconds>.<base64url HMAC-SHA256(secret, purpose + expiry)>
//
// Anyone with the secret could mint cookies, so it lives in a secret store
// in production. Rotating the secret logs every browser out.

// SessionCookie is the name of the owner session cookie.
const SessionCookie = "photosync_owner"

// DefaultSessionTTL is how long an owner session lasts.
const DefaultSessionTTL = 7 * 24 * time.Hour

// MinSessionSecretLen is the minimum length of Config.SessionSecret.
const MinSessionSecretLen = 32

// sessionPurpose binds the MAC to this use, so the secret can't be abused to
// produce MACs meaningful anywhere else.
const sessionPurpose = "photosync owner session v1\n"

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

func (s *Server) sessionMAC(exp string) string {
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(sessionPurpose + exp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// newSession returns a session cookie value valid until exp.
func (s *Server) newSession(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return "v1." + e + "." + s.sessionMAC(e)
}

// validSession reports whether v is an unexpired session cookie value
// signed with this server's secret.
func (s *Server) validSession(v string) bool {
	version, rest, ok := strings.Cut(v, ".")
	if !ok || version != "v1" {
		return false
	}
	e, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || s.now().Unix() >= exp {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(s.sessionMAC(e)))
}

// sessionCookie returns the cookie to set. A non-positive maxAge deletes it.
// HttpOnly keeps it from page scripts; SameSite=Strict keeps browsers from
// sending it on cross-site requests; Secure keeps it off plain HTTP (browsers
// treat http://localhost as secure, so local development still works).
func sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}
