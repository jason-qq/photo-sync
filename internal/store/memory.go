package store

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"photo-sync/internal/model"
)

// Memory is an in-process Store. Data is lost when the process exits.
type Memory struct {
	mu        sync.Mutex
	sessions  map[string]model.Session
	photos    map[string]map[string]model.Photo    // sessionID -> photoID -> photo
	hashes    map[string]map[string]string         // sessionID -> hash -> photoID
	reviewers map[string]map[string]model.Reviewer // sessionID -> reviewerID -> reviewer
	tokens    map[string][2]string                 // tokenHash -> {sessionID, reviewerID}
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		sessions:  map[string]model.Session{},
		photos:    map[string]map[string]model.Photo{},
		hashes:    map[string]map[string]string{},
		reviewers: map[string]map[string]model.Reviewer{},
		tokens:    map[string][2]string{},
	}
}

func (m *Memory) CreateSession(_ context.Context, s model.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return ErrExists
	}
	m.sessions[s.ID] = s
	m.photos[s.ID] = map[string]model.Photo{}
	m.hashes[s.ID] = map[string]string{}
	m.reviewers[s.ID] = map[string]model.Reviewer{}
	return nil
}

func (m *Memory) GetSession(_ context.Context, id string) (model.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return model.Session{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) ListSessions(_ context.Context) ([]model.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions := make([]model.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	slices.SortFunc(sessions, func(a, b model.Session) int { return strings.Compare(a.ID, b.ID) })
	return sessions, nil
}

func (m *Memory) CreatePhoto(_ context.Context, p model.Photo) (model.Photo, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[p.SessionID]; !ok {
		return model.Photo{}, false, ErrNotFound
	}
	if pid, ok := m.hashes[p.SessionID][p.Hash]; ok {
		return m.photos[p.SessionID][pid], false, nil
	}
	if _, ok := m.photos[p.SessionID][p.ID]; ok {
		return model.Photo{}, false, ErrExists
	}
	m.photos[p.SessionID][p.ID] = p
	m.hashes[p.SessionID][p.Hash] = p.ID
	return p, true, nil
}

func (m *Memory) GetPhoto(_ context.Context, sessionID, photoID string) (model.Photo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.photos[sessionID][photoID]
	if !ok {
		return model.Photo{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) ListPhotos(_ context.Context, sessionID string) ([]model.Photo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	photos := make([]model.Photo, 0, len(m.photos[sessionID]))
	for _, p := range m.photos[sessionID] {
		photos = append(photos, p)
	}
	// Sort on Dynamo's LSI1 sort key itself so both stores agree exactly,
	// including filenames containing characters that sort before '#'.
	slices.SortFunc(photos, func(a, b model.Photo) int {
		return strings.Compare(photoLSI1SK(a.Filename, a.ID), photoLSI1SK(b.Filename, b.ID))
	})
	return photos, nil
}

func (m *Memory) SetPhotoStatus(_ context.Context, sessionID, photoID string, status model.PhotoStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.photos[sessionID][photoID]
	if !ok {
		return ErrNotFound
	}
	p.Status = status
	m.photos[sessionID][photoID] = p
	return nil
}

func (m *Memory) CreateReviewer(_ context.Context, r model.Reviewer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[r.SessionID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.tokens[r.TokenHash]; ok {
		return ErrExists
	}
	if _, ok := m.reviewers[r.SessionID][r.ID]; ok {
		return ErrExists
	}
	m.reviewers[r.SessionID][r.ID] = r
	m.tokens[r.TokenHash] = [2]string{r.SessionID, r.ID}
	return nil
}

func (m *Memory) GetReviewer(_ context.Context, sessionID, reviewerID string) (model.Reviewer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reviewers[sessionID][reviewerID]
	if !ok {
		return model.Reviewer{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) GetReviewerByTokenHash(_ context.Context, tokenHash string) (model.Reviewer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref, ok := m.tokens[tokenHash]
	if !ok {
		return model.Reviewer{}, ErrNotFound
	}
	return m.reviewers[ref[0]][ref[1]], nil
}

func (m *Memory) ListReviewers(_ context.Context, sessionID string) ([]model.Reviewer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return nil, ErrNotFound
	}
	rs := make([]model.Reviewer, 0, len(m.reviewers[sessionID]))
	for _, r := range m.reviewers[sessionID] {
		rs = append(rs, r)
	}
	slices.SortFunc(rs, func(a, b model.Reviewer) int { return strings.Compare(a.ID, b.ID) })
	return rs, nil
}

func (m *Memory) SetReviewerExpiry(_ context.Context, sessionID, reviewerID string, expiresAt time.Time) error {
	return m.updateReviewer(sessionID, reviewerID, func(r *model.Reviewer) { r.ExpiresAt = expiresAt })
}

func (m *Memory) RevokeReviewer(_ context.Context, sessionID, reviewerID string) error {
	return m.updateReviewer(sessionID, reviewerID, func(r *model.Reviewer) { r.Revoked = true })
}

func (m *Memory) updateReviewer(sessionID, reviewerID string, update func(*model.Reviewer)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reviewers[sessionID][reviewerID]
	if !ok {
		return ErrNotFound
	}
	update(&r)
	m.reviewers[sessionID][reviewerID] = r
	return nil
}
