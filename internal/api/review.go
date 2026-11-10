package api

import "time"

// Reviewer-facing routes. They don't use the owner API key: the reviewer's
// link token is exchanged for a reviewer session cookie, which every other
// reviewer route requires.
//
// Status codes the reviewer UI relies on:
//   - 401: no valid reviewer session cookie (open the link again).
//   - 403: the link is unknown, expired or revoked (ask for a new one).
//
// Responses never include filenames, hashes or other reviewers' data.
const (
	RouteReviewerSession = "POST /api/review/session"
	RouteReviewerView    = "GET /api/review"
)

// ReviewerSessionRequest is the body of POST /api/review/session. The
// reviewer page reads the token from its link's URL fragment
// (ReviewLinkPath) and posts it here once.
//
// Responses: 204 with Set-Cookie; 400 invalid body; 403 invalid link.
type ReviewerSessionRequest struct {
	Token string `json:"token"`
}

// MaxTokenLen bounds ReviewerSessionRequest.Token (real tokens are 43
// characters).
const MaxTokenLen = 128

// Validate checks r.
func (r ReviewerSessionRequest) Validate() error {
	if r.Token == "" || len(r.Token) > MaxTokenLen {
		return invalid("token", "required")
	}
	return nil
}

// ReviewerViewResponse is the body of GET /api/review: everything the
// reviewer page shows.
//
// Responses: 200; 401 no session; 403 link no longer valid.
type ReviewerViewResponse struct {
	Session  ReviewSession `json:"session"`
	Reviewer ReviewerSelf  `json:"reviewer"`
	// Photos are the session's ready photos in shot order. Never null.
	Photos []ReviewPhoto `json:"photos"`
}

// ReviewSession is the session as a reviewer sees it.
type ReviewSession struct {
	Title string `json:"title"`
	Date  string `json:"date"` // YYYY-MM-DD
}

// ReviewerSelf is the reviewer's own record, shown as "Reviewing as …".
type ReviewerSelf struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ReviewPhoto is a photo as a reviewer sees it: an ID and image URLs only.
// The URLs expire (about an hour); re-fetch the view for fresh ones.
type ReviewPhoto struct {
	ID     string    `json:"id"`
	Images ImageURLs `json:"images"`
}
