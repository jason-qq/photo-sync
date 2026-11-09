// Package review implements the photo review use cases: creating sessions,
// registering photos (with per-session dedupe), and confirming uploads.
//
// It owns the business rules and knows nothing about HTTP or JSON. Inputs
// are assumed to have passed wire-level validation (internal/api).
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"photo-sync/internal/blob"
	"photo-sync/internal/model"
	"photo-sync/internal/store"
)

// DefaultUploadTTL is how long presigned upload URLs stay valid.
const DefaultUploadTTL = 15 * time.Minute

// Errors returned by Service. Messages are safe to show to clients.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrPhotoNotFound   = errors.New("photo not found")
	// ErrNotUploaded is wrapped with the rendition name, e.g.
	// "thumb has not been uploaded".
	ErrNotUploaded = errors.New("has not been uploaded")
)

// Config holds the service's dependencies.
type Config struct {
	Store     store.Store
	Blobs     blob.Store
	UploadTTL time.Duration    // defaults to DefaultUploadTTL
	Now       func() time.Time // defaults to time.Now
}

// Service implements the review use cases.
type Service struct {
	store     store.Store
	blobs     blob.Store
	uploadTTL time.Duration
	now       func() time.Time
}

// New returns a Service. Store and Blobs are required.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Blobs == nil {
		return nil, errors.New("review: Store and Blobs are required")
	}
	s := &Service{store: cfg.Store, blobs: cfg.Blobs, uploadTTL: cfg.UploadTTL, now: cfg.Now}
	if s.uploadTTL == 0 {
		s.uploadTTL = DefaultUploadTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// CreateSession creates a session. The title is stored trimmed.
func (s *Service) CreateSession(ctx context.Context, title, date string) (model.Session, error) {
	sess := model.Session{
		ID:        model.NewID(),
		Title:     strings.TrimSpace(title),
		Date:      date,
		CreatedAt: s.now().UTC(),
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return model.Session{}, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// Uploads holds presigned URLs for uploading a photo's renditions.
type Uploads struct {
	Thumb   string
	Preview string
}

// PhotoRegistration is the result of RegisterPhoto.
type PhotoRegistration struct {
	Photo model.Photo
	// Created is false if the session already had a photo with this hash.
	Created bool
	// Uploads is set when the client should upload renditions: for new
	// photos, existing photos still pending (resuming an interrupted
	// upload), and any existing photo when forced.
	Uploads *Uploads
}

// RegisterPhoto registers a raw file in a session, deduplicated by hash.
// A new photo gets upload URLs. An existing ready photo is returned as-is,
// or with fresh upload URLs (to overwrite its renditions) if force is set.
// An existing pending photo always gets upload URLs so it can be finished.
func (s *Service) RegisterPhoto(ctx context.Context, sessionID, filename, hash string, force bool) (PhotoRegistration, error) {
	p, created, err := s.store.CreatePhoto(ctx, model.Photo{
		ID:        model.NewID(),
		SessionID: sessionID,
		Filename:  filename,
		Hash:      hash,
		Status:    model.StatusPending,
		CreatedAt: s.now().UTC(),
	})
	if errors.Is(err, store.ErrNotFound) {
		return PhotoRegistration{}, ErrSessionNotFound
	}
	if err != nil {
		return PhotoRegistration{}, fmt.Errorf("create photo: %w", err)
	}

	reg := PhotoRegistration{Photo: p, Created: created}
	if created || force || p.Status == model.StatusPending {
		if reg.Uploads, err = s.presignUploads(ctx, p); err != nil {
			return PhotoRegistration{}, err
		}
	}
	return reg, nil
}

func (s *Service) presignUploads(ctx context.Context, p model.Photo) (*Uploads, error) {
	thumb, err := s.blobs.PresignPut(ctx, p.ThumbKey(), s.uploadTTL)
	if err != nil {
		return nil, fmt.Errorf("presign thumb: %w", err)
	}
	preview, err := s.blobs.PresignPut(ctx, p.PreviewKey(), s.uploadTTL)
	if err != nil {
		return nil, fmt.Errorf("presign preview: %w", err)
	}
	return &Uploads{Thumb: thumb, Preview: preview}, nil
}

// CompletePhoto verifies both renditions were uploaded and marks the photo
// ready. It is idempotent.
func (s *Service) CompletePhoto(ctx context.Context, sessionID, photoID string) (model.Photo, error) {
	p, err := s.store.GetPhoto(ctx, sessionID, photoID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Photo{}, ErrPhotoNotFound
	}
	if err != nil {
		return model.Photo{}, fmt.Errorf("get photo: %w", err)
	}

	renditions := []struct{ name, key string }{
		{"thumb", p.ThumbKey()},
		{"preview", p.PreviewKey()},
	}
	for _, r := range renditions {
		ok, err := s.blobs.Exists(ctx, r.key)
		if err != nil {
			return model.Photo{}, fmt.Errorf("check %s: %w", r.name, err)
		}
		if !ok {
			return model.Photo{}, fmt.Errorf("%s %w", r.name, ErrNotUploaded)
		}
	}

	if err := s.store.SetPhotoStatus(ctx, p.SessionID, p.ID, model.StatusReady); err != nil {
		return model.Photo{}, fmt.Errorf("mark ready: %w", err)
	}
	p.Status = model.StatusReady
	return p, nil
}
