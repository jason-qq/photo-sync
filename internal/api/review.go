package api

import (
	"net/url"
	"time"
	"unicode/utf8"
)

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
	RouteSaveFeedback    = "PATCH /api/review/photos/{pid}/feedback"
	RouteDeleteFeedback  = "DELETE /api/review/photos/{pid}/feedback"
)

// FeedbackPath is the path for RouteSaveFeedback and RouteDeleteFeedback.
func FeedbackPath(photoID string) string {
	return "/api/review/photos/" + url.PathEscape(photoID) + "/feedback"
}

// Verdict values.
const (
	VerdictAccept = "accept"
	VerdictReject = "reject"
)

// MaxCommentLen bounds feedback comments, in characters (runes), so
// Japanese gets the same allowance as English.
const MaxCommentLen = 2000

// FeedbackRequest is the body of PATCH /api/review/photos/{pid}/feedback: a
// partial update of the caller's feedback on one photo. Fields are
// independent (a rejected photo may be starred). For each field: absent
// leaves it unchanged, null clears it, a value sets it. At least one field
// must be present. Clearing every field removes the feedback.
//
// Responses: 200 Feedback; 400 invalid body; 401/403 see above; 404 photo
// not found (or not ready). DELETE on the same path removes all of the
// caller's feedback on the photo: 204, idempotent.
type FeedbackRequest struct {
	Verdict Optional[string] `json:"verdict"` // VerdictAccept or VerdictReject
	Stars   Optional[int]    `json:"stars"`   // 1–5
	// Comment is free text, at most MaxCommentLen characters. An empty
	// string clears it, like null.
	Comment Optional[string] `json:"comment"`
}

// Feedback is the caller's feedback on one photo. Unset fields are null.
type Feedback struct {
	Verdict   *string    `json:"verdict"`
	Stars     *int       `json:"stars"`
	Comment   *string    `json:"comment"`
	UpdatedAt *time.Time `json:"updatedAt"` // null if there is no feedback
}

// Validate checks r.
func (r FeedbackRequest) Validate() error {
	if !r.Verdict.Set && !r.Stars.Set && !r.Comment.Set {
		return invalid("feedback", "at least one of verdict, stars, comment is required")
	}
	if v := r.Verdict.Value; v != nil && *v != VerdictAccept && *v != VerdictReject {
		return invalid("verdict", `must be "accept", "reject" or null`)
	}
	if n := r.Stars.Value; n != nil && (*n < 1 || *n > 5) {
		return invalid("stars", "must be 1-5 or null")
	}
	if c := r.Comment.Value; c != nil && utf8.RuneCountInString(*c) > MaxCommentLen {
		return invalid("comment", "at most 2000 characters")
	}
	return nil
}

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

// ReviewPhoto is a photo as a reviewer sees it: an ID, image URLs and the
// reviewer's own feedback. The URLs expire (about an hour); re-fetch the
// view for fresh ones.
type ReviewPhoto struct {
	ID     string    `json:"id"`
	Images ImageURLs `json:"images"`
	// Feedback is absent if the reviewer hasn't reviewed this photo.
	Feedback *Feedback `json:"feedback,omitempty"`
}
