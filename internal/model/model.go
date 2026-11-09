// Package model defines the domain types shared by the API and the uploader.
package model

import (
	"time"

	"github.com/oklog/ulid/v2"
)

// NewID returns a new ULID string. IDs are always minted by the server.
func NewID() string {
	return ulid.Make().String()
}

// Session is one review set for one model.
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Date      string    `json:"date"` // YYYY-MM-DD
	CreatedAt time.Time `json:"createdAt"`
}

// PhotoStatus tracks whether a photo's renditions have been uploaded.
type PhotoStatus string

const (
	StatusPending PhotoStatus = "pending"
	StatusReady   PhotoStatus = "ready"
)

// Photo is one raw file's previews within a session. Uniqueness within a
// session is by Hash (SHA-256 of the raw file, lowercase hex).
type Photo struct {
	ID        string      `json:"id"`
	SessionID string      `json:"sessionId"`
	Filename  string      `json:"filename"`
	Hash      string      `json:"hash"`
	Status    PhotoStatus `json:"status"`
	CreatedAt time.Time   `json:"createdAt"`
}

// ThumbKey is the blob key of the photo's grid thumbnail.
func (p Photo) ThumbKey() string {
	return "sessions/" + p.SessionID + "/" + p.ID + "/thumb.jpg"
}

// PreviewKey is the blob key of the photo's judging preview.
func (p Photo) PreviewKey() string {
	return "sessions/" + p.SessionID + "/" + p.ID + "/preview.jpg"
}
