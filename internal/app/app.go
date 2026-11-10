// Package app assembles the complete HTTP application (JSON API, owner
// web UI and, for local blob storage, the blob endpoint) from its
// dependencies. It is the single composition root: cmd/api, the Lambda
// entry point and tests all build the app here, and differ only in which
// store and blob implementations they pass in.
//
// Choosing implementations (reading the environment, connecting to AWS)
// is the caller's job; app only wires what it is given.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"photo-sync/internal/blob"
	"photo-sync/internal/review"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
	"photo-sync/internal/ui"
)

// Config holds the app's dependencies.
type Config struct {
	APIKey string // owner API key (required)
	Store  store.Store
	Blobs  blob.Store
	Logger *slog.Logger // defaults to slog.Default()
}

// New returns the app's HTTP handler:
//
//	/api/        JSON API (internal/server)
//	/dev/blob/   signed blob uploads/downloads, only when Blobs is a *blob.Local
//	/            owner web UI (internal/ui)
func New(cfg Config) (http.Handler, error) {
	if cfg.Store == nil || cfg.Blobs == nil {
		return nil, errors.New("app: Store and Blobs are required")
	}
	svc, err := review.New(review.Config{Store: cfg.Store, Blobs: cfg.Blobs})
	if err != nil {
		return nil, err
	}
	srv, err := server.New(server.Config{Service: svc, APIKey: cfg.APIKey, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	// The UI's CSP must allow images from wherever the blob store's view
	// URLs point (the server's own origin for Local, the bucket for S3).
	imageOrigin, err := urlOrigin(cfg.Blobs)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", srv)
	if local, ok := cfg.Blobs.(*blob.Local); ok {
		mux.Handle(blob.LocalPathPrefix+"{key...}", local)
	}
	mux.Handle("/", ui.Handler(ui.Config{ImageOrigins: []string{imageOrigin}}))
	return mux, nil
}

// urlOrigin returns the scheme://host that blobs' view URLs point at.
func urlOrigin(blobs blob.Store) (string, error) {
	raw, err := blobs.PresignGet(context.Background(), "origin-probe.jpg", time.Minute)
	if err != nil {
		return "", fmt.Errorf("app: presign probe URL: %w", err)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("app: blob URL %q has no origin", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}
