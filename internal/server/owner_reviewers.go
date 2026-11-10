package server

import (
	"net/http"
	"time"

	"photo-sync/internal/api"
	"photo-sync/internal/model"
)

// createReviewer handles api.RouteCreateReviewer.
func (s *Server) createReviewer(w http.ResponseWriter, r *http.Request) error {
	var req api.CreateReviewerRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	var exp time.Time
	if req.ExpiresAt != nil {
		exp = *req.ExpiresAt
	}
	link, err := s.svc.CreateReviewer(r.Context(), r.PathValue("sid"), req.Name, exp)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, api.CreateReviewerResponse{
		Reviewer: s.toAPIReviewer(link.Reviewer),
		LinkPath: api.ReviewLinkPath(link.Token),
	})
	return nil
}

// listReviewers handles api.RouteListReviewers.
func (s *Server) listReviewers(w http.ResponseWriter, r *http.Request) error {
	rs, err := s.svc.ListReviewers(r.Context(), r.PathValue("sid"))
	if err != nil {
		return err
	}
	resp := api.ListReviewersResponse{Reviewers: make([]api.Reviewer, len(rs))}
	for i, rv := range rs {
		resp.Reviewers[i] = s.toAPIReviewer(rv)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// extendReviewer handles api.RouteExtendReviewer.
func (s *Server) extendReviewer(w http.ResponseWriter, r *http.Request) error {
	var req api.ExtendReviewerRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	rv, err := s.svc.ExtendReviewer(r.Context(), r.PathValue("sid"), r.PathValue("rid"), req.ExpiresAt)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.toAPIReviewer(rv))
	return nil
}

// revokeReviewer handles api.RouteRevokeReviewer.
func (s *Server) revokeReviewer(w http.ResponseWriter, r *http.Request) error {
	rv, err := s.svc.RevokeReviewer(r.Context(), r.PathValue("sid"), r.PathValue("rid"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.toAPIReviewer(rv))
	return nil
}

func (s *Server) toAPIReviewer(r model.Reviewer) api.Reviewer {
	return api.Reviewer{
		ID:        r.ID,
		Name:      r.Name,
		ExpiresAt: r.ExpiresAt,
		Revoked:   r.Revoked,
		Active:    r.Active(s.now()),
		CreatedAt: r.CreatedAt,
	}
}
