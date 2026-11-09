// Package api defines the HTTP wire contract shared by the API server and
// its clients (the uploader CLI): routes, request and response bodies, and
// request validation.
//
// It is deliberately independent of internal/model so the wire format can
// evolve (and later be generated from an OpenAPI spec) without changing
// domain types.
//
// Authentication: owner routes require "Authorization: Bearer <api key>".
// Errors: non-2xx responses carry an Error body.
package api

import (
	"net/url"
	"time"
)

// Routes, as used in the server's mux patterns.
const (
	RouteCreateSession = "POST /api/sessions"
	RouteCreatePhoto   = "POST /api/sessions/{sid}/photos"
	RouteCompletePhoto = "POST /api/sessions/{sid}/photos/{pid}/complete"
)

// QueryForce, set to "true" on create-photo, requests fresh upload URLs for
// an existing photo so its renditions can be overwritten.
const QueryForce = "force"

// SessionsPath is the path for RouteCreateSession.
func SessionsPath() string { return "/api/sessions" }

// PhotosPath is the path for RouteCreatePhoto.
func PhotosPath(sessionID string) string {
	return "/api/sessions/" + url.PathEscape(sessionID) + "/photos"
}

// CompletePath is the path for RouteCompletePhoto.
func CompletePath(sessionID, photoID string) string {
	return PhotosPath(sessionID) + "/" + url.PathEscape(photoID) + "/complete"
}

// CreateSessionRequest is the body of POST /api/sessions.
//
// Responses: 201 Session; 400 invalid body.
type CreateSessionRequest struct {
	// Title is required, 1–MaxTitleLen characters after trimming whitespace.
	Title string `json:"title"`
	// Date is the shoot date, YYYY-MM-DD.
	Date string `json:"date"`
}

// Session describes a review session.
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Date      string    `json:"date"` // YYYY-MM-DD
	CreatedAt time.Time `json:"createdAt"`
}

// CreatePhotoRequest is the body of POST /api/sessions/{sid}/photos.
//
// Responses:
//   - 201 PhotoResponse with UploadURLs: new photo.
//   - 200 PhotoResponse: the session already has a photo with this Hash. With
//     ?force=true, UploadURLs is included to overwrite its renditions.
//   - 400 invalid body; 404 session not found.
type CreatePhotoRequest struct {
	// Filename is the raw file's base name (no path separators),
	// 1–MaxFilenameLen characters. Shown only to the owner.
	Filename string `json:"filename"`
	// Hash is the lowercase hex SHA-256 of the raw file. Photos are unique
	// per session by Hash.
	Hash string `json:"hash"`
}

// PhotoStatus is a photo's upload state.
type PhotoStatus string

const (
	// PhotoPending means renditions are not yet confirmed uploaded.
	PhotoPending PhotoStatus = "pending"
	// PhotoReady means both renditions are uploaded and the photo is visible
	// to reviewers.
	PhotoReady PhotoStatus = "ready"
)

// UploadURLs are presigned URLs for uploading a photo's renditions. Each
// accepts a single HTTP PUT of a JPEG body and expires after a short TTL.
type UploadURLs struct {
	Thumb   string `json:"thumb"`
	Preview string `json:"preview"`
}

// PhotoResponse is returned by create-photo and complete-photo.
type PhotoResponse struct {
	PhotoID    string      `json:"photoId"`
	Status     PhotoStatus `json:"status"`
	UploadURLs *UploadURLs `json:"uploadUrls,omitempty"`
}

// POST /api/sessions/{sid}/photos/{pid}/complete has no body. It verifies
// both renditions were uploaded and marks the photo ready.
//
// Responses: 200 PhotoResponse (idempotent); 404 photo not found;
// 409 a rendition has not been uploaded.

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
