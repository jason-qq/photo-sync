// Command uploader uploads a folder of raw files (Sony ARW, Nikon NEF),
// including those in its subfolders, to a review session. Raws never leave
// the machine: only resized, EXIF-free JPEG previews are uploaded.
//
// Usage:
//
//	uploader [flags] <folder>
//
// The first run creates a session and writes .review-manifest.json into the
// folder; later runs reuse that session and skip files already uploaded.
//
// Environment:
//
//	PHOTOSYNC_API_KEY   owner API key (required)
//	PHOTOSYNC_API_URL   API base URL (default http://localhost:8080)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"photo-sync/internal/apiclient"
	"photo-sync/internal/uploader"
)

func main() {
	os.Exit(run())
}

func run() int {
	title := flag.String("title", "", "session title for a new session (default: folder name)")
	date := flag.String("date", "", "session date YYYY-MM-DD for a new session (default: today)")
	force := flag.Bool("force", false, "re-upload previews for photos already uploaded")
	workers := flag.Int("workers", 0, "files to process in parallel (default: min(4, CPUs))")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: uploader [flags] <folder>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return 2
	}

	apiKey := os.Getenv("PHOTOSYNC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: PHOTOSYNC_API_KEY must be set")
		return 2
	}
	apiURL := os.Getenv("PHOTOSYNC_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}

	// Ctrl+C cancels in-flight requests and stops handing out new files.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	sum, err := uploader.Run(ctx, uploader.Config{
		Dir:     flag.Arg(0),
		Client:  apiclient.New(apiURL, apiKey),
		APIURL:  apiURL,
		Title:   *title,
		Date:    *date,
		Force:   *force,
		Workers: *workers,
		Out:     os.Stdout,
	})
	if sum.SessionID != "" {
		fmt.Printf("\ndone in %v: %d uploaded, %d skipped, %d failed\n",
			time.Since(start).Round(100*time.Millisecond), sum.Uploaded, sum.Skipped, sum.Failed)
	}
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "interrupted; rerun to continue")
		return 130
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	case sum.Failed > 0:
		return 1
	}
	return 0
}
