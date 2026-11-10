// Package store persists sessions and photos. Store is the interface the API
// depends on; Memory is an in-process implementation for local development
// and tests. A DynamoDB implementation will follow the single-table design.
package store

import (
	"context"
	"errors"

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

	// CreatePhoto stores p unless the session already has a photo with the
	// same Hash. In that case it returns the existing photo and created=false.
	// The check-and-insert is atomic. Returns ErrNotFound if the session
	// doesn't exist.
	CreatePhoto(ctx context.Context, p model.Photo) (stored model.Photo, created bool, err error)
	GetPhoto(ctx context.Context, sessionID, photoID string) (model.Photo, error)
	// ListPhotos returns all photos in a session, in ID order (which is
	// creation order, since IDs are ULIDs). Returns ErrNotFound if the
	// session doesn't exist.
	ListPhotos(ctx context.Context, sessionID string) ([]model.Photo, error)
	SetPhotoStatus(ctx context.Context, sessionID, photoID string, status model.PhotoStatus) error
}
