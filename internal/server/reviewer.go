package server

import (
	"context"
	"errors"
	"net/http"

	"photo-sync/internal/api"
	"photo-sync/internal/model"
	"photo-sync/internal/review"
)

// Routes called by the reviewer (see api/review.go). Owner routes for
// managing reviewer links are in owner_reviewers.go.

// reviewerKey is the context key under which requireReviewer stores the
// authenticated reviewer.
type reviewerKey struct{}

func reviewerFrom(ctx context.Context) model.Reviewer {
	return ctx.Value(reviewerKey{}).(model.Reviewer)
}

// requireReviewer admits requests carrying a valid reviewer session cookie
// whose link is still active, and makes the reviewer available to the
// handler via reviewerFrom. The reviewer record is read on every request,
// so revocation and expiry apply immediately.
func (s *Server) requireReviewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(ReviewerCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no review session")
			return
		}
		sid, rid, ok := s.reviewerSession(c.Value)
		if !ok {
			writeError(w, http.StatusUnauthorized, "no review session")
			return
		}
		rv, err := s.svc.ActiveReviewer(r.Context(), sid, rid)
		if err != nil {
			s.fail(w, r, err) // ErrInvalidLink → 403
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reviewerKey{}, rv)))
	})
}

// startReviewerSession handles api.RouteReviewerSession: exchanges a link
// token for a reviewer session cookie.
func (s *Server) startReviewerSession(w http.ResponseWriter, r *http.Request) error {
	var req api.ReviewerSessionRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	rv, err := s.svc.AuthenticateReviewer(r.Context(), req.Token)
	if err != nil {
		return err
	}
	exp := s.now().Add(ReviewerSessionTTL)
	value := s.newReviewerSession(exp, rv.SessionID, rv.ID)
	http.SetCookie(w, authCookie(ReviewerCookie, value, int(ReviewerSessionTTL.Seconds())))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// reviewerView handles api.RouteReviewerView.
func (s *Server) reviewerView(w http.ResponseWriter, r *http.Request) error {
	rv := reviewerFrom(r.Context())
	v, err := s.svc.ReviewerView(r.Context(), rv)
	if errors.Is(err, review.ErrSessionNotFound) {
		// The session is gone (e.g. deleted later): the link is dead too.
		return review.ErrInvalidLink
	}
	if err != nil {
		return err
	}
	resp := api.ReviewerViewResponse{
		Session:  api.ReviewSession{Title: v.Session.Title, Date: v.Session.Date},
		Reviewer: api.ReviewerSelf{Name: rv.Name, ExpiresAt: rv.ExpiresAt},
		Photos:   make([]api.ReviewPhoto, len(v.Photos)),
	}
	for i, p := range v.Photos {
		resp.Photos[i] = api.ReviewPhoto{
			ID:     p.Photo.ID,
			Images: api.ImageURLs{Thumb: p.Images.Thumb, Preview: p.Images.Preview},
		}
	}
	// The response holds signed image URLs; don't let anything cache it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
	return nil
}
