// Package uploader uploads a folder of raw files to a review session:
// hash → register → generate previews → upload → complete, for many files
// concurrently, recording progress in the folder's manifest.
package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"photo-sync/internal/api"
	"photo-sync/internal/apiclient"
	"photo-sync/internal/manifest"
	"photo-sync/internal/preview"
)

// Config controls a Run.
type Config struct {
	Dir    string
	Client *apiclient.Client
	// APIURL is recorded in the manifest and checked on later runs.
	APIURL string

	Title string // new session title; defaults to the folder name
	Date  string // new session date (YYYY-MM-DD); defaults to today
	Force bool   // re-upload renditions of photos that are already ready

	// Workers is how many files are processed at once. Each worker holds a
	// raw file and its decoded preview in memory (~100 MB for a 24 MP raw).
	// Defaults to min(4, NumCPU).
	Workers int
	Out     io.Writer // progress output; defaults to io.Discard
}

// Summary reports what a Run did.
type Summary struct {
	SessionID string
	Uploaded  int
	Skipped   int
	Failed    int
}

// rawExts are the supported raw file extensions (lowercase).
var rawExts = map[string]bool{".arw": true, ".nef": true}

// Run uploads every raw file in cfg.Dir. Per-file failures are counted in
// the Summary and don't stop the run; the returned error is for problems
// with the run as a whole (bad manifest, session creation, cancellation).
func Run(ctx context.Context, cfg Config) (Summary, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = min(4, runtime.NumCPU())
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}

	files, err := findRaws(cfg.Dir)
	if err != nil {
		return Summary{}, err
	}
	m, err := manifest.Load(cfg.Dir)
	if err != nil {
		return Summary{}, err
	}
	sessionID, err := ensureSession(ctx, cfg, m)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{SessionID: sessionID}
	fmt.Fprintf(cfg.Out, "session %s: %d raw file(s), %d worker(s)\n", sessionID, len(files), cfg.Workers)

	// A fatal error (e.g. bad API key) cancels this context to stop the
	// whole run instead of failing every file the same way.
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fatal error

	// Worker pool:
	//
	//   producer ──jobs──► worker × N ──results──► collector (this goroutine)
	//
	// Only the collector touches the manifest and output, so neither needs
	// a lock. Each worker owns one file at a time end to end.
	jobs := make(chan string)
	results := make(chan result)

	go func() { // producer
		defer close(jobs) // ends the workers' range loops
		for _, f := range files {
			select {
			case jobs <- f:
			case <-ctx.Done(): // cancelled: stop handing out work
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() {
			for path := range jobs {
				results <- process(ctx, cfg, sessionID, path)
			}
		})
	}
	go func() { // close results once every worker has finished
		wg.Wait()
		close(results)
	}()

	var saveErr error
	for r := range results { // collector
		if fatal != nil {
			continue // drain remaining results; they fail from cancellation
		}
		if isFatal(r.err) {
			fatal = r.err
			cancel()
			continue
		}
		note := ""
		if r.err == nil {
			if prev, ok := m.Photos[r.name]; ok && prev.Hash != r.hash {
				note = " (file changed since last run)"
			}
			m.Photos[r.name] = manifest.Entry{PhotoID: r.photoID, Hash: r.hash}
			if err := m.Save(cfg.Dir); err != nil && saveErr == nil {
				saveErr = fmt.Errorf("save manifest: %w", err)
			}
		}
		switch {
		case r.err != nil:
			sum.Failed++
			fmt.Fprintf(cfg.Out, "FAILED      %s: %v\n", r.name, r.err)
		case r.uploaded:
			sum.Uploaded++
			fmt.Fprintf(cfg.Out, "%-11s %s (%v)%s\n", r.action, r.name, r.dur.Round(time.Millisecond), note)
		default:
			sum.Skipped++
			fmt.Fprintf(cfg.Out, "skipped     %s (already uploaded)%s\n", r.name, note)
		}
	}

	if fatal != nil {
		return sum, fatal
	}
	if err := parent.Err(); err != nil {
		return sum, err
	}
	return sum, saveErr
}

// isFatal reports whether err means no file can succeed, so the run should
// stop: the API rejected our credentials.
func isFatal(err error) bool {
	var ae *apiclient.APIError
	return errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden)
}

// ensureSession returns the folder's session, creating it (and saving the
// manifest) on first run.
func ensureSession(ctx context.Context, cfg Config, m *manifest.Manifest) (string, error) {
	if m.SessionID != "" {
		if m.APIURL != cfg.APIURL {
			return "", fmt.Errorf("folder was uploaded to %s, not %s; delete %s to start a new session",
				m.APIURL, cfg.APIURL, manifest.FileName)
		}
		return m.SessionID, nil
	}

	title := cfg.Title
	if title == "" {
		abs, err := filepath.Abs(cfg.Dir)
		if err != nil {
			return "", err
		}
		title = filepath.Base(abs)
	}
	date := cfg.Date
	if date == "" {
		date = time.Now().Format(time.DateOnly)
	}
	req := api.CreateSessionRequest{Title: title, Date: date}
	if err := req.Validate(); err != nil {
		return "", err
	}
	sess, err := cfg.Client.CreateSession(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	// Save immediately so a crash later can't orphan this session.
	m.SessionID, m.APIURL = sess.ID, cfg.APIURL
	if err := m.Save(cfg.Dir); err != nil {
		return "", fmt.Errorf("save manifest: %w", err)
	}
	return sess.ID, nil
}

// result is one file's outcome, sent from a worker to the collector.
type result struct {
	name     string
	hash     string
	photoID  string
	uploaded bool   // false means skipped
	action   string // "uploaded" or "re-uploaded"
	dur      time.Duration
	err      error
}

// process runs the full pipeline for one raw file.
func process(ctx context.Context, cfg Config, sessionID, path string) result {
	start := time.Now()
	r := result{name: filepath.Base(path)}
	fail := func(step string, err error) result {
		r.err = fmt.Errorf("%s: %w", step, err)
		return r
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fail("read", err)
	}
	sum := sha256.Sum256(raw)
	r.hash = hex.EncodeToString(sum[:])

	reg, created, err := cfg.Client.RegisterPhoto(ctx, sessionID,
		api.CreatePhotoRequest{Filename: r.name, Hash: r.hash}, cfg.Force)
	if err != nil {
		return fail("register", err)
	}
	r.photoID = reg.PhotoID
	if reg.UploadURLs == nil {
		return r // already uploaded and ready
	}

	thumb, prev, err := preview.Generate(raw)
	if err != nil {
		return fail("preview", err)
	}
	// Upload both renditions concurrently. If one fails, gctx is cancelled
	// so the other stops early; Wait returns the first error.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return uploadRendition(gctx, cfg.Client, "thumb", reg.UploadURLs.Thumb, thumb) })
	g.Go(func() error { return uploadRendition(gctx, cfg.Client, "preview", reg.UploadURLs.Preview, prev) })
	if err := g.Wait(); err != nil {
		return fail("upload", err)
	}
	if _, err := cfg.Client.CompletePhoto(ctx, sessionID, reg.PhotoID); err != nil {
		return fail("complete", err)
	}

	r.uploaded, r.action, r.dur = true, "uploaded", time.Since(start)
	if !created {
		r.action = "re-uploaded" // forced, or resuming an interrupted upload
	}
	return r
}

func uploadRendition(ctx context.Context, c *apiclient.Client, name, url string, jpeg []byte) error {
	if err := c.Upload(ctx, url, jpeg); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// findRaws lists supported raw files in dir (not recursive), sorted by name.
func findRaws(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !rawExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	if len(files) == 0 {
		return nil, errors.New("no .ARW or .NEF files found")
	}
	return files, nil
}
