package server

import (
	"net/http"

	"photo-sync/internal/api"
	"photo-sync/internal/model"
	"photo-sync/internal/review"
)

// createSession handles api.RouteCreateSession.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) error {
	var req api.CreateSessionRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	sess, err := s.svc.CreateSession(r.Context(), req.Title, req.Date)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, toAPISession(sess))
	return nil
}

// createPhoto handles api.RouteCreatePhoto.
func (s *Server) createPhoto(w http.ResponseWriter, r *http.Request) error {
	var req api.CreatePhotoRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		return err
	}
	force := r.URL.Query().Get(api.QueryForce) == "true"

	reg, err := s.svc.RegisterPhoto(r.Context(), r.PathValue("sid"), req.Filename, req.Hash, force)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if reg.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toAPIPhoto(reg.Photo, reg.Uploads))
	return nil
}

// completePhoto handles api.RouteCompletePhoto.
func (s *Server) completePhoto(w http.ResponseWriter, r *http.Request) error {
	p, err := s.svc.CompletePhoto(r.Context(), r.PathValue("sid"), r.PathValue("pid"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toAPIPhoto(p, nil))
	return nil
}

// Domain → wire conversions.

func toAPISession(s model.Session) api.Session {
	return api.Session{ID: s.ID, Title: s.Title, Date: s.Date, CreatedAt: s.CreatedAt}
}

func toAPIPhoto(p model.Photo, u *review.Uploads) api.PhotoResponse {
	resp := api.PhotoResponse{PhotoID: p.ID, Status: api.PhotoStatus(p.Status)}
	if u != nil {
		resp.UploadURLs = &api.UploadURLs{Thumb: u.Thumb, Preview: u.Preview}
	}
	return resp
}
