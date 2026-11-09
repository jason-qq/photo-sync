// Package server implements the photo review HTTP API. It translates between
// HTTP/JSON (internal/api) and the review use cases (internal/review).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"photo-sync/internal/api"
	"photo-sync/internal/review"
)

// maxBodySize caps JSON request bodies.
const maxBodySize = 1 << 20

// Config holds the server's dependencies.
type Config struct {
	Service *review.Service
	APIKey  string       // owner API key, sent as "Authorization: Bearer <key>"
	Logger  *slog.Logger // defaults to slog.Default()
}

// Server is an http.Handler serving the API routes.
type Server struct {
	svc    *review.Service
	apiKey []byte
	log    *slog.Logger
	mux    *http.ServeMux
}

// New returns a Server. It returns an error if required config is missing.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("server: Service is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("server: APIKey is required")
	}
	s := &Server{
		svc:    cfg.Service,
		apiKey: []byte(cfg.APIKey),
		log:    cfg.Logger,
		mux:    http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.Handle(api.RouteCreateSession, s.requireOwner(s.handle(s.createSession)))
	s.mux.Handle(api.RouteCreatePhoto, s.requireOwner(s.handle(s.createPhoto)))
	s.mux.Handle(api.RouteCompletePhoto, s.requireOwner(s.handle(s.completePhoto)))
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

// requireOwner rejects requests without the owner API key.
func (s *Server) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(key), s.apiKey) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
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
	case errors.Is(err, review.ErrSessionNotFound), errors.Is(err, review.ErrPhotoNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, review.ErrNotUploaded):
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
