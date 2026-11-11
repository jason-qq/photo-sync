// Package api defines the HTTP wire contract shared by the API server and
// its clients (the uploader CLI): routes, request and response bodies, and
// request validation.
//
// It is deliberately independent of internal/model so the wire format can
// evolve (and later be generated from an OpenAPI spec) without changing
// domain types.
//
// Authentication: owner routes require "Authorization: Bearer <api key>"
// or the session cookie set by POST /api/login.
// Errors: non-2xx responses carry an Error body.
package api

import (
	"net/url"
	"time"
)

// Routes, as used in the server's mux patterns.
const (
	RouteLogin         = "POST /api/login"
	RouteLogout        = "POST /api/logout"
	RouteCreateSession = "POST /api/sessions"
	RouteListSessions  = "GET /api/sessions"
	RouteCreatePhoto   = "POST /api/sessions/{sid}/photos"
	RouteListPhotos    = "GET /api/sessions/{sid}/photos"
	RouteCompletePhoto = "POST /api/sessions/{sid}/photos/{pid}/complete"
)

// QueryForce, set to "true" on create-photo, requests fresh upload URLs for
// an existing photo so its renditions can be overwritten.
const QueryForce = "force"

// SessionsPath is the path for RouteCreateSession and RouteListSessions.
func SessionsPath() string { return "/api/sessions" }

// PhotosPath is the path for RouteCreatePhoto and RouteListPhotos.
func PhotosPath(sessionID string) string {
	return "/api/sessions/" + url.PathEscape(sessionID) + "/photos"
}

// CompletePath is the path for RouteCompletePhoto.
func CompletePath(sessionID, photoID string) string {
	return PhotosPath(sessionID) + "/" + url.PathEscape(photoID) + "/complete"
}

// LoginRequest is the body of POST /api/login, used by the web UI to
// exchange the owner API key for an HttpOnly session cookie (so page
// scripts never hold the key). Owner routes accept that cookie in place of
// the Authorization header.
//
// Responses: 204 with Set-Cookie; 400 invalid body; 401 wrong key.
// POST /api/logout (no body) always returns 204 and deletes the cookie.
type LoginRequest struct {
	APIKey string `json:"apiKey"`
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

// ListSessionsResponse is the body of GET /api/sessions.
//
// Responses: 200 ListSessionsResponse.
type ListSessionsResponse struct {
	// Sessions are sorted by Date, newest first. Never null.
	Sessions []Session `json:"sessions"`
}

// CreatePhotoRequest is the body of POST /api/sessions/{sid}/photos.
//
// Responses:
//   - 201 PhotoResponse with UploadURLs: new photo.
//   - 200 PhotoResponse: the session already has a photo with this Hash.
//     UploadURLs is included if the photo is still pending (resume an
//     interrupted upload) or with ?force=true (overwrite its renditions).
//   - 400 invalid body; 404 session not found.
type CreatePhotoRequest struct {
	// Filename is the raw file's path relative to the session folder, with
	// "/" between folder names (e.g. "101MSDCF/DSC00001.ARW"; just the name
	// for files at the top level), 1–MaxFilenameLen characters. Photos sort
	// by it, so rollover folders keep shot order. Shown only to the owner.
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

// ListPhotosResponse is the body of GET /api/sessions/{sid}/photos. It is an
// object (not a bare array) so per-session data such as feedback can be
// added later without breaking clients.
//
// Responses: 200 ListPhotosResponse; 404 session not found.
type ListPhotosResponse struct {
	// Photos are sorted by Filename (a relative path). Never null.
	Photos []Photo `json:"photos"`
}

// Photo is a photo as the owner sees it.
type Photo struct {
	ID        string      `json:"id"`
	Filename  string      `json:"filename"`
	Hash      string      `json:"hash"`
	Status    PhotoStatus `json:"status"`
	CreatedAt time.Time   `json:"createdAt"`
	// Images is present only when Status is ready.
	Images *ImageURLs `json:"images,omitempty"`
	// Feedback holds each reviewer's feedback on this photo, ordered by
	// reviewer. Reviewers without feedback have no entry. Never null.
	Feedback []ReviewerFeedback `json:"feedback"`
}

// ReviewerFeedback is one reviewer's feedback on a photo, as the owner sees
// it. ReviewerID matches Reviewer.ID from GET /api/sessions/{sid}/reviewers.
type ReviewerFeedback struct {
	ReviewerID string `json:"reviewerId"`
	Feedback
}

// ImageURLs are presigned URLs for viewing a photo's renditions via HTTP
// GET. They expire (about an hour); re-fetch the list to get fresh ones.
type ImageURLs struct {
	Thumb   string `json:"thumb"`
	Preview string `json:"preview"`
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
