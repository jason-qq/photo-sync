package server

import (
	"errors"
	"net/http"
	"strings"

	"photo-sync/internal/api"
	"photo-sync/internal/model"
	"photo-sync/internal/store"
)

// createSession handles api.RouteCreateSession.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSessionRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}

	sess := model.Session{
		ID:        model.NewID(),
		Title:     strings.TrimSpace(req.Title),
		Date:      req.Date,
		CreatedAt: s.now().UTC(),
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPISession(sess))
}

// createPhoto handles api.RouteCreatePhoto.
func (s *Server) createPhoto(w http.ResponseWriter, r *http.Request) {
	var req api.CreatePhotoRequest
	if err := decodeAndValidate(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	force := r.URL.Query().Get(api.QueryForce) == "true"

	p, created, err := s.store.CreatePhoto(r.Context(), model.Photo{
		ID:        model.NewID(),
		SessionID: r.PathValue("sid"),
		Filename:  req.Filename,
		Hash:      req.Hash,
		Status:    model.StatusPending,
		CreatedAt: s.now().UTC(),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := toAPIPhoto(p)
	if created || force {
		if resp.UploadURLs, err = s.presignUploads(r, p); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

func (s *Server) presignUploads(r *http.Request, p model.Photo) (*api.UploadURLs, error) {
	thumb, err := s.blobs.PresignPut(r.Context(), p.ThumbKey(), s.uploadTTL)
	if err != nil {
		return nil, err
	}
	preview, err := s.blobs.PresignPut(r.Context(), p.PreviewKey(), s.uploadTTL)
	if err != nil {
		return nil, err
	}
	return &api.UploadURLs{Thumb: thumb, Preview: preview}, nil
}

// completePhoto handles api.RouteCompletePhoto.
func (s *Server) completePhoto(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPhoto(r.Context(), r.PathValue("sid"), r.PathValue("pid"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "photo not found")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	for name, key := range map[string]string{"thumb": p.ThumbKey(), "preview": p.PreviewKey()} {
		ok, err := s.blobs.Exists(r.Context(), key)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !ok {
			writeError(w, http.StatusConflict, name+" has not been uploaded")
			return
		}
	}

	if err := s.store.SetPhotoStatus(r.Context(), p.SessionID, p.ID, model.StatusReady); err != nil {
		s.fail(w, r, err)
		return
	}
	p.Status = model.StatusReady
	writeJSON(w, http.StatusOK, toAPIPhoto(p))
}

// Domain → wire conversions.

func toAPISession(s model.Session) api.Session {
	return api.Session{ID: s.ID, Title: s.Title, Date: s.Date, CreatedAt: s.CreatedAt}
}

func toAPIPhoto(p model.Photo) api.PhotoResponse {
	return api.PhotoResponse{PhotoID: p.ID, Status: api.PhotoStatus(p.Status)}
}
