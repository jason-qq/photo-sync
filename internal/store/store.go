// Package store persists sessions, photos and reviewers. Store is the
// interface the API depends on; Memory is an in-process implementation for
// local development and tests, Dynamo the DynamoDB single-table one.
package store

import (
	"context"
	"errors"
	"time"

	"photo-sync/internal/model"
)

var (
	// ErrNotFound is returned when a session or photo does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrExists is returned when creating an item whose ID is already taken.
	ErrExists = errors.New("store: already exists")
)

// Store is the persistence interface used by the API.
type Store interface {
	CreateSession(ctx context.Context, s model.Session) error
	GetSession(ctx context.Context, id string) (model.Session, error)
	// ListSessions returns all sessions in ID (creation) order.
	ListSessions(ctx context.Context) ([]model.Session, error)

	// CreatePhoto stores p unless the session already has a photo with the
	// same Hash. In that case it returns the existing photo and created=false.
	// The check-and-insert is atomic. Returns ErrNotFound if the session
	// doesn't exist.
	CreatePhoto(ctx context.Context, p model.Photo) (stored model.Photo, created bool, err error)
	GetPhoto(ctx context.Context, sessionID, photoID string) (model.Photo, error)
	// ListPhotos returns all photos in a session sorted by Filename, ties
	// broken by ID (precisely: by the string "PHOTO#{filename}#{id}", which
	// is DynamoDB's index sort key). Sorting belongs to the store so that a
	// paginated version returns correctly ordered pages. Returns
	// ErrNotFound if the session doesn't exist.
	ListPhotos(ctx context.Context, sessionID string) ([]model.Photo, error)
	SetPhotoStatus(ctx context.Context, sessionID, photoID string, status model.PhotoStatus) error

	// CreateReviewer stores r. Returns ErrNotFound if the session doesn't
	// exist and ErrExists if r.ID or r.TokenHash is already taken. Atomic:
	// on error nothing is stored.
	CreateReviewer(ctx context.Context, r model.Reviewer) error
	GetReviewer(ctx context.Context, sessionID, reviewerID string) (model.Reviewer, error)
	// GetReviewerByTokenHash finds the reviewer whose link token hashes to
	// tokenHash, revoked or expired ones included; callers check Active.
	GetReviewerByTokenHash(ctx context.Context, tokenHash string) (model.Reviewer, error)
	// ListReviewers returns a session's reviewers in ID (creation) order.
	// Returns ErrNotFound if the session doesn't exist.
	ListReviewers(ctx context.Context, sessionID string) ([]model.Reviewer, error)
	// SetReviewerExpiry and RevokeReviewer return ErrNotFound if the
	// reviewer doesn't exist.
	SetReviewerExpiry(ctx context.Context, sessionID, reviewerID string, expiresAt time.Time) error
	RevokeReviewer(ctx context.Context, sessionID, reviewerID string) error

	// UpdateFeedback applies patch to a reviewer's feedback on a photo,
	// creating it if needed, and sets UpdatedAt. The update is atomic, so
	// concurrent patches to different fields don't overwrite each other.
	// It returns the feedback as stored afterwards. Callers check that the
	// photo and reviewer exist.
	UpdateFeedback(ctx context.Context, sessionID, reviewerID, photoID string, patch model.FeedbackPatch, updatedAt time.Time) (model.Feedback, error)
	// DeleteFeedback removes a reviewer's feedback on a photo. Deleting
	// feedback that doesn't exist is not an error.
	DeleteFeedback(ctx context.Context, sessionID, reviewerID, photoID string) error
	// ListFeedback returns one reviewer's feedback in a session, in photo
	// ID order.
	ListFeedback(ctx context.Context, sessionID, reviewerID string) ([]model.Feedback, error)
	// ListSessionFeedback returns every reviewer's feedback in a session,
	// ordered by reviewer ID then photo ID. For the owner only.
	ListSessionFeedback(ctx context.Context, sessionID string) ([]model.Feedback, error)
}
