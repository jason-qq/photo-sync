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

// listSessions handles api.RouteListSessions.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) error {
	sessions, err := s.svc.ListSessions(r.Context())
	if err != nil {
		return err
	}
	resp := api.ListSessionsResponse{Sessions: make([]api.Session, len(sessions))}
	for i, sess := range sessions {
		resp.Sessions[i] = toAPISession(sess)
	}
	writeJSON(w, http.StatusOK, resp)
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

// listPhotos handles api.RouteListPhotos.
func (s *Server) listPhotos(w http.ResponseWriter, r *http.Request) error {
	sid := r.PathValue("sid")
	views, err := s.svc.ListPhotos(r.Context(), sid)
	if err != nil {
		return err
	}
	feedback, err := s.svc.SessionFeedback(r.Context(), sid)
	if err != nil {
		return err
	}
	resp := api.ListPhotosResponse{Photos: make([]api.Photo, len(views))}
	for i, v := range views {
		resp.Photos[i] = toAPIPhotoView(v, feedback[v.Photo.ID])
	}
	writeJSON(w, http.StatusOK, resp)
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

func toAPIPhotoView(v review.PhotoView, feedback []model.Feedback) api.Photo {
	p := api.Photo{
		ID:        v.Photo.ID,
		Filename:  v.Photo.Filename,
		Hash:      v.Photo.Hash,
		Status:    api.PhotoStatus(v.Photo.Status),
		CreatedAt: v.Photo.CreatedAt,
		Feedback:  make([]api.ReviewerFeedback, len(feedback)),
	}
	if v.Images != nil {
		p.Images = &api.ImageURLs{Thumb: v.Images.Thumb, Preview: v.Images.Preview}
	}
	for i, f := range feedback {
		p.Feedback[i] = api.ReviewerFeedback{ReviewerID: f.ReviewerID, Feedback: toAPIFeedback(f)}
	}
	return p
}
