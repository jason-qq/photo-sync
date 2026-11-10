// Command api runs the photo review API as a local HTTP server, using the
// in-memory store and disk-backed blob storage.
//
// Environment:
//
//	PHOTOSYNC_API_KEY   owner API key (required)
//	PHOTOSYNC_ADDR      listen address (default localhost:8080)
//	PHOTOSYNC_BASE_URL  externally reachable origin (default http://<addr>)
//	PHOTOSYNC_DATA_DIR  blob storage directory (default ./data)
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"photo-sync/internal/blob"
	"photo-sync/internal/review"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	apiKey := os.Getenv("PHOTOSYNC_API_KEY")
	if apiKey == "" {
		return errors.New("PHOTOSYNC_API_KEY must be set")
	}
	addr := envOr("PHOTOSYNC_ADDR", "localhost:8080")
	baseURL := envOr("PHOTOSYNC_BASE_URL", "http://"+addr)
	dataDir := envOr("PHOTOSYNC_DATA_DIR", "data")

	// Upload URLs are signed with a per-process secret; they only need to
	// outlive their 15-minute TTL, not a restart.
	secret := make([]byte, 32)
	rand.Read(secret)

	blobs := blob.NewLocal(dataDir, baseURL, secret)
	svc, err := review.New(review.Config{Store: store.NewMemory(), Blobs: blobs})
	if err != nil {
		return err
	}
	srv, err := server.New(server.Config{Service: svc, APIKey: apiKey})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/", srv)
	mux.Handle(blob.LocalPathPrefix+"{key...}", blobs)

	hs := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // uploads
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr, "baseURL", baseURL, "dataDir", dataDir)
		errc <- hs.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(shutdownCtx)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests logs method, path (never the query, which may carry upload
// signatures), status, and duration.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}
