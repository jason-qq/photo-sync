// Command api runs the photo review API and owner web UI as an HTTP server.
// Open the base URL in a browser and sign in with the API key.
//
// Sessions and photos are kept in memory (lost on restart) unless
// PHOTOSYNC_DYNAMODB_TABLE is set. Preview images are stored on local disk
// (served by this process under /dev/blob/) unless PHOTOSYNC_S3_BUCKET is
// set.
//
// Environment:
//
//	PHOTOSYNC_API_KEY            owner API key (required); only its hash is kept
//	PHOTOSYNC_SESSION_SECRET     signs browser session cookies, >= 32 bytes
//	                             (default: random per process)
//	PHOTOSYNC_ADDR               listen address (default localhost:8080)
//	PHOTOSYNC_BASE_URL           externally reachable origin (default http://<addr>)
//	PHOTOSYNC_DATA_DIR           local blob directory (default ./data)
//	PHOTOSYNC_DYNAMODB_TABLE     use this DynamoDB table instead of memory
//	PHOTOSYNC_DYNAMODB_ENDPOINT  DynamoDB endpoint override, for DynamoDB Local
//	                             (e.g. http://localhost:8000); the table is
//	                             created if missing and dummy credentials are
//	                             used.
//	PHOTOSYNC_S3_BUCKET          store images in this S3 bucket instead of disk
//	PHOTOSYNC_S3_ENDPOINT        S3-compatible server instead of AWS (e.g.
//	                             RustFS at http://localhost:9000); the bucket
//	                             is created if missing. Credentials come from
//	                             AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
//
// Without endpoint overrides, AWS is used via the default credential chain
// (AWS_PROFILE, AWS_REGION, ...) and the table/bucket must already exist.
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
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"photo-sync/internal/app"
	"photo-sync/internal/blob"
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

	st, err := openStore()
	if err != nil {
		return err
	}
	blobs, err := openBlobs(dataDir, baseURL)
	if err != nil {
		return err
	}
	handler, err := app.New(app.Config{
		APIKeyHash:    server.HashAPIKey(apiKey),
		SessionSecret: sessionSecret(),
		Store:         st,
		Blobs:         blobs,
	})
	if err != nil {
		return err
	}

	hs := &http.Server{
		Addr:              addr,
		Handler:           logRequests(handler),
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

// sessionSecret returns PHOTOSYNC_SESSION_SECRET, or a random per-process
// secret if it isn't set (browser sessions then end when the API restarts).
func sessionSecret() []byte {
	if s := os.Getenv("PHOTOSYNC_SESSION_SECRET"); s != "" {
		return []byte(s) // server.New rejects one that is too short
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	slog.Info("session secret: random per process (sign-ins end on restart)")
	return secret
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
		cfg, err := awsConfig()
		if err != nil {
			return nil, err
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

// openBlobs returns the blob store selected by the environment: S3 if
// PHOTOSYNC_S3_BUCKET is set, local disk otherwise.
func openBlobs(dataDir, baseURL string) (blob.Store, error) {
	bucket := os.Getenv("PHOTOSYNC_S3_BUCKET")
	if bucket == "" {
		// Local URLs are signed with a per-process secret; they only need
		// to outlive their TTL, not a restart.
		secret := make([]byte, 32)
		rand.Read(secret)
		slog.Info("blobs: local disk", "dir", dataDir)
		return blob.NewLocal(dataDir, baseURL, secret), nil
	}

	cfg, err := awsConfig()
	if err != nil {
		return nil, err
	}
	endpoint := os.Getenv("PHOTOSYNC_S3_ENDPOINT")
	if endpoint != "" && cfg.Region == "" {
		cfg.Region = "us-east-1" // local servers ignore it, but the SDK requires one
	}
	client := blob.NewS3Client(cfg, endpoint)
	if endpoint != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := blob.CreateBucket(ctx, client, bucket); err != nil {
			return nil, err
		}
	}
	slog.Info("blobs: S3", "bucket", bucket, "endpoint", endpoint, "region", cfg.Region)
	return blob.NewS3(client, bucket), nil
}

// awsConfig loads the default AWS configuration (credentials and region
// from the environment, AWS_PROFILE, etc.) once, on first use.
var awsConfig = sync.OnceValues(func() (aws.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	return cfg, nil
})

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
