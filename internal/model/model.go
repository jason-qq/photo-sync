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

// Reviewer is one person's access to one session via a secret link. The
// link's token is never stored, only its SHA-256 (TokenHash). ID is stable
// across the reviewer's lifetime and keys their feedback.
type Reviewer struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Name      string    `json:"name"`
	TokenHash string    `json:"-"` // lowercase hex SHA-256 of the link token
	ExpiresAt time.Time `json:"expiresAt"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"createdAt"`
}

// Active reports whether the reviewer's link works at time now.
func (r Reviewer) Active(now time.Time) bool {
	return !r.Revoked && now.Before(r.ExpiresAt)
}

// Verdict is a reviewer's primary judgement of a photo.
type Verdict string

const (
	VerdictNone   Verdict = ""
	VerdictAccept Verdict = "accept"
	VerdictReject Verdict = "reject"
)

// Feedback is one reviewer's feedback on one photo. All fields are
// independent and optional (DESIGN.md §7): a rejected photo may still be
// starred. Zero values mean "not set".
type Feedback struct {
	SessionID  string
	ReviewerID string
	PhotoID    string
	Verdict    Verdict
	Stars      int // 1–5, 0 = none
	Comment    string
	UpdatedAt  time.Time
}

// Empty reports whether no field is set.
func (f Feedback) Empty() bool {
	return f.Verdict == VerdictNone && f.Stars == 0 && f.Comment == ""
}

// FeedbackPatch is a partial update to Feedback. A nil field is left as
// is; a non-nil field is set, and a zero value (VerdictNone, 0, "") clears
// it.
type FeedbackPatch struct {
	Verdict *Verdict
	Stars   *int
	Comment *string
}

// ThumbKey is the blob key of the photo's grid thumbnail.
func (p Photo) ThumbKey() string {
	return "sessions/" + p.SessionID + "/" + p.ID + "/thumb.jpg"
}

// PreviewKey is the blob key of the photo's judging preview.
func (p Photo) PreviewKey() string {
	return "sessions/" + p.SessionID + "/" + p.ID + "/preview.jpg"
}
