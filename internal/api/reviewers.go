package api

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Owner routes for managing a session's reviewer links.
const (
	RouteCreateReviewer = "POST /api/sessions/{sid}/reviewers"
	RouteListReviewers  = "GET /api/sessions/{sid}/reviewers"
	RouteExtendReviewer = "PATCH /api/sessions/{sid}/reviewers/{rid}"
	RouteRevokeReviewer = "DELETE /api/sessions/{sid}/reviewers/{rid}"
)

// ReviewersPath is the path for RouteCreateReviewer and RouteListReviewers.
func ReviewersPath(sessionID string) string {
	return "/api/sessions/" + url.PathEscape(sessionID) + "/reviewers"
}

// ReviewerPath is the path for RouteExtendReviewer and RouteRevokeReviewer.
func ReviewerPath(sessionID, reviewerID string) string {
	return ReviewersPath(sessionID) + "/" + url.PathEscape(reviewerID)
}

// ReviewLinkPath returns the path of a reviewer's link for token. The token
// is in the fragment, which browsers never send to the server; prefix the
// app's origin to get the shareable URL.
func ReviewLinkPath(token string) string { return "/r#" + token }

// MaxReviewerNameLen bounds reviewer names, in characters (not bytes, so
// Japanese names get the same allowance).
const MaxReviewerNameLen = 100

// CreateReviewerRequest is the body of POST /api/sessions/{sid}/reviewers.
//
// Responses: 201 CreateReviewerResponse; 400 invalid body or expiry;
// 404 session not found.
type CreateReviewerRequest struct {
	// Name identifies the reviewer to the owner and is shown to them
	// ("Reviewing as …"). Required, 1–MaxReviewerNameLen characters after
	// trimming whitespace.
	Name string `json:"name"`
	// ExpiresAt is when the link stops working. Optional: defaults to 7
	// days from now; must be in the future and at most 90 days ahead.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// CreateReviewerResponse is returned once, when a link is created. The
// token can't be retrieved again: only its hash is stored.
type CreateReviewerResponse struct {
	Reviewer Reviewer `json:"reviewer"`
	// LinkPath is ReviewLinkPath(token): append it to the app's origin to
	// get the link to send to the reviewer.
	LinkPath string `json:"linkPath"`
}

// ExtendReviewerRequest is the body of PATCH /api/sessions/{sid}/reviewers/{rid}.
//
// Responses: 200 Reviewer; 400 invalid expiry; 404 reviewer not found;
// 409 the link is revoked (create a new one instead).
type ExtendReviewerRequest struct {
	// ExpiresAt replaces the expiry (same bounds as on create). An expired
	// link becomes active again.
	ExpiresAt time.Time `json:"expiresAt"`
}

// DELETE /api/sessions/{sid}/reviewers/{rid} revokes the link permanently
// (idempotent). The reviewer and their feedback are kept.
// Responses: 200 Reviewer; 404 reviewer not found.

// ListReviewersResponse is the body of GET /api/sessions/{sid}/reviewers.
//
// Responses: 200; 404 session not found.
type ListReviewersResponse struct {
	// Reviewers, oldest first. Never null.
	Reviewers []Reviewer `json:"reviewers"`
}

// Reviewer is a reviewer link as the owner sees it.
type Reviewer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
	Revoked   bool      `json:"revoked"`
	// Active is true if the link works right now (not revoked, not expired).
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
	// Progress, only in GET …/reviewers (absent in other responses):
	// Reviewed is how many photos have any feedback from this reviewer;
	// LastActivity is when they last saved feedback, absent if never.
	Reviewed     *int       `json:"reviewed,omitempty"`
	LastActivity *time.Time `json:"lastActivity,omitempty"`
}

// Validate checks r. Name is checked after trimming; callers store it
// trimmed. Expiry bounds are checked by the service (they depend on now).
func (r CreateReviewerRequest) Validate() error {
	if n := utf8.RuneCountInString(strings.TrimSpace(r.Name)); n == 0 || n > MaxReviewerNameLen {
		return invalid("name", "required, at most 100 characters")
	}
	return nil
}

// Validate checks r.
func (r ExtendReviewerRequest) Validate() error {
	if r.ExpiresAt.IsZero() {
		return invalid("expiresAt", "required, RFC 3339 timestamp")
	}
	return nil
}
