// Command api runs the photo review API and owner web UI as a local HTTP
// server with disk-backed blob storage. Open the base URL in a browser and
// sign in with the API key.
//
// Sessions and photos are kept in memory (lost on restart) unless
// PHOTOSYNC_DYNAMODB_TABLE is set.
//
// Environment:
//
//	PHOTOSYNC_API_KEY            owner API key (required)
//	PHOTOSYNC_ADDR               listen address (default localhost:8080)
//	PHOTOSYNC_BASE_URL           externally reachable origin (default http://<addr>)
//	PHOTOSYNC_DATA_DIR           blob storage directory (default ./data)
//	PHOTOSYNC_DYNAMODB_TABLE     use this DynamoDB table instead of memory
//	PHOTOSYNC_DYNAMODB_ENDPOINT  DynamoDB endpoint override, for DynamoDB Local
//	                             (e.g. http://localhost:8000); the table is
//	                             created if missing and dummy credentials are
//	                             used. Unset: real AWS via the default
//	                             credential chain (AWS_PROFILE, AWS_REGION, ...).
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"photo-sync/internal/blob"
	"photo-sync/internal/review"
	"photo-sync/internal/server"
	"photo-sync/internal/store"
	"photo-sync/internal/ui"
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

	st, err := openStore()
	if err != nil {
		return err
	}
	blobs := blob.NewLocal(dataDir, baseURL, secret)
	svc, err := review.New(review.Config{Store: st, Blobs: blobs})
	if err != nil {
		return err
	}
	srv, err := server.New(server.Config{Service: svc, APIKey: apiKey})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", srv)
	mux.Handle(blob.LocalPathPrefix+"{key...}", blobs)
	// Images come from the blob URLs, which use baseURL; allow it in case
	// the UI is opened via a different host name (e.g. 127.0.0.1).
	mux.Handle("/", ui.Handler(ui.Config{ImageOrigins: []string{baseURL}}))

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

// openStore returns the store selected by the environment.
func openStore() (store.Store, error) {
	table := os.Getenv("PHOTOSYNC_DYNAMODB_TABLE")
	if table == "" {
		slog.Info("store: in memory (data is lost on restart)")
		return store.NewMemory(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	endpoint := os.Getenv("PHOTOSYNC_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("load AWS config: %w", err)
		}
		slog.Info("store: DynamoDB", "table", table, "region", cfg.Region)
		return store.NewDynamo(dynamodb.NewFromConfig(cfg), table), nil
	}

	// DynamoDB Local accepts any credentials and region.
	db := dynamodb.New(dynamodb.Options{
		Region:       envOr("AWS_REGION", "us-east-1"),
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	if err := store.CreateTable(ctx, db, table); err != nil {
		return nil, err
	}
	slog.Info("store: DynamoDB Local", "table", table, "endpoint", endpoint)
	return store.NewDynamo(db, table), nil
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
