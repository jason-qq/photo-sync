// Package server implements the photo review HTTP API.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"photo-sync/internal/api"
	"photo-sync/internal/blob"
	"photo-sync/internal/store"
)

// DefaultUploadTTL is how long presigned upload URLs stay valid.
const DefaultUploadTTL = 15 * time.Minute

// maxBodySize caps JSON request bodies.
const maxBodySize = 1 << 20

// Config holds the server's dependencies.
type Config struct {
	Store  store.Store
	Blobs  blob.Store
	APIKey string // owner API key, sent as "Authorization: Bearer <key>"

	UploadTTL time.Duration    // defaults to DefaultUploadTTL
	Logger    *slog.Logger     // defaults to slog.Default()
	Now       func() time.Time // defaults to time.Now
}

// Server is an http.Handler serving the API routes.
type Server struct {
	store     store.Store
	blobs     blob.Store
	apiKey    []byte
	uploadTTL time.Duration
	log       *slog.Logger
	now       func() time.Time
	mux       *http.ServeMux
}

// New returns a Server. It returns an error if required config is missing.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Blobs == nil {
		return nil, errors.New("server: Store and Blobs are required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("server: APIKey is required")
	}
	s := &Server{
		store:     cfg.Store,
		blobs:     cfg.Blobs,
		apiKey:    []byte(cfg.APIKey),
		uploadTTL: cfg.UploadTTL,
		log:       cfg.Logger,
		now:       cfg.Now,
		mux:       http.NewServeMux(),
	}
	if s.uploadTTL == 0 {
		s.uploadTTL = DefaultUploadTTL
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
	s.mux.Handle(api.RouteCreateSession, s.requireOwner(s.createSession))
	s.mux.Handle(api.RouteCreatePhoto, s.requireOwner(s.createPhoto))
	s.mux.Handle(api.RouteCompletePhoto, s.requireOwner(s.completePhoto))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// requireOwner rejects requests without the owner API key.
func (s *Server) requireOwner(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(key), s.apiKey) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	})
}

// httpError is an error with a status code and a client-safe message.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// fail writes err to the client: httpErrors as-is, validation errors as 400,
// anything else as a logged 500 with a generic message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeError(w, he.status, he.msg)
		return
	}
	var ve *api.ValidationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, ve.Error())
		return
	}
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
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

// decodeAndValidate decodes the JSON body into v and validates it.
func decodeAndValidate(w http.ResponseWriter, r *http.Request, v validator) error {
	if err := decodeJSON(w, r, v); err != nil {
		return err
	}
	return v.Validate()
}

// decodeJSON decodes exactly one JSON object into v, rejecting unknown
// fields and oversized bodies.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodySize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &httpError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return badRequest("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("request body must be a single JSON object")
	}
	return nil
}
