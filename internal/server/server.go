// Package server implements the photo review HTTP API. It translates between
// HTTP/JSON (internal/api) and the review use cases (internal/review).
package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"photo-sync/internal/api"
	"photo-sync/internal/review"
)

// maxBodySize caps JSON request bodies.
const maxBodySize = 1 << 20

// Config holds the server's dependencies.
type Config struct {
	Service *review.Service
	// APIKeyHash is HashAPIKey(owner API key). Clients send the key as
	// "Authorization: Bearer <key>", or exchange it for a session cookie.
	APIKeyHash []byte
	// SessionSecret signs session cookies; at least MinSessionSecretLen
	// bytes, and the same on every instance.
	SessionSecret []byte
	SessionTTL    time.Duration    // defaults to DefaultSessionTTL
	Logger        *slog.Logger     // defaults to slog.Default()
	Now           func() time.Time // defaults to time.Now
}

// Server is an http.Handler serving the API routes.
type Server struct {
	svc           *review.Service
	apiKeyHash    []byte
	sessionSecret []byte
	sessionTTL    time.Duration
	log           *slog.Logger
	now           func() time.Time
	mux           *http.ServeMux
}

// New returns a Server. It returns an error if required config is missing.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("server: Service is required")
	}
	if len(cfg.APIKeyHash) != sha256.Size {
		return nil, errors.New("server: APIKeyHash must be a SHA-256 hash")
	}
	if len(cfg.SessionSecret) < MinSessionSecretLen {
		return nil, fmt.Errorf("server: SessionSecret must be at least %d bytes", MinSessionSecretLen)
	}
	s := &Server{
		svc:           cfg.Service,
		apiKeyHash:    cfg.APIKeyHash,
		sessionSecret: cfg.SessionSecret,
		sessionTTL:    cfg.SessionTTL,
		log:           cfg.Logger,
		now:           cfg.Now,
		mux:           http.NewServeMux(),
	}
	if s.sessionTTL == 0 {
		s.sessionTTL = DefaultSessionTTL
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.Handle(api.RouteLogin, s.handle(s.login))
	s.mux.Handle(api.RouteLogout, s.handle(s.logout))
	s.mux.Handle(api.RouteCreateSession, s.requireOwner(s.handle(s.createSession)))
	s.mux.Handle(api.RouteListSessions, s.requireOwner(s.handle(s.listSessions)))
	s.mux.Handle(api.RouteCreatePhoto, s.requireOwner(s.handle(s.createPhoto)))
	s.mux.Handle(api.RouteListPhotos, s.requireOwner(s.handle(s.listPhotos)))
	s.mux.Handle(api.RouteCompletePhoto, s.requireOwner(s.handle(s.completePhoto)))
	s.mux.Handle(api.RouteCreateReviewer, s.requireOwner(s.handle(s.createReviewer)))
	s.mux.Handle(api.RouteListReviewers, s.requireOwner(s.handle(s.listReviewers)))
	s.mux.Handle(api.RouteExtendReviewer, s.requireOwner(s.handle(s.extendReviewer)))
	s.mux.Handle(api.RouteRevokeReviewer, s.requireOwner(s.handle(s.revokeReviewer)))

	s.mux.Handle(api.RouteReviewerSession, s.handle(s.startReviewerSession))
	s.mux.Handle(api.RouteReviewerView, s.requireReviewer(s.handle(s.reviewerView)))
	s.mux.Handle(api.RouteSaveFeedback, s.requireReviewer(s.handle(s.saveFeedback)))
	s.mux.Handle(api.RouteDeleteFeedback, s.requireReviewer(s.handle(s.deleteFeedback)))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handlerFunc is an HTTP handler that reports failure by returning an error
// instead of writing an error response itself.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts h to http.Handler, writing any returned error via fail.
func (s *Server) handle(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	})
}

// requireOwner rejects requests that don't carry the owner API key
// (Authorization: Bearer, used by the CLI) or a valid session cookie (used
// by the browser). If an Authorization header is present it alone decides;
// a bad key isn't rescued by a cookie.
func (s *Server) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isOwner(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
	})
}

func (s *Server) isOwner(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); h != "" {
		key, ok := strings.CutPrefix(h, "Bearer ")
		return ok && s.validKey(key)
	}
	c, err := r.Cookie(SessionCookie)
	return err == nil && s.validSession(c.Value)
}

// login handles api.RouteLogin: exchanges the API key for a session cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var req api.LoginRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	if !s.validKey(req.APIKey) {
		return &httpError{http.StatusUnauthorized, "invalid API key"}
	}
	exp := s.now().Add(s.sessionTTL)
	http.SetCookie(w, authCookie(SessionCookie, s.newSession(exp), int(s.sessionTTL.Seconds())))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// logout handles api.RouteLogout: deletes the session cookie. The cookie
// is stateless, so a copy taken earlier stays valid until it expires.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	http.SetCookie(w, authCookie(SessionCookie, "", -1))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// httpError is an error with a status code and a client-safe message, for
// failures detected at the HTTP layer (e.g. malformed JSON).
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// fail maps err to an HTTP error response. This is the single place where
// errors become status codes; unrecognized errors are logged and hidden
// behind a generic 500.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	var ve *api.ValidationError
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.msg)
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Error())
	case errors.Is(err, review.ErrInvalidExpiry), errors.Is(err, review.ErrInvalidFeedback):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, review.ErrInvalidLink):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, review.ErrSessionNotFound), errors.Is(err, review.ErrPhotoNotFound),
		errors.Is(err, review.ErrReviewerNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, review.ErrNotUploaded), errors.Is(err, review.ErrReviewerRevoked):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

// validator is implemented by api request types.
type validator interface {
	Validate() error
}

// decodeAndValidate decodes exactly one JSON object into v, rejecting
// unknown fields and oversized bodies, then validates it.
func decodeAndValidate(w http.ResponseWriter, r *http.Request, v validator) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &httpError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &httpError{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &httpError{http.StatusBadRequest, "request body must be a single JSON object"}
	}
	return v.Validate()
}
