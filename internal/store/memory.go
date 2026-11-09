package store

import (
	"context"
	"sync"

	"photo-sync/internal/model"
)

// Memory is an in-process Store. Data is lost when the process exits.
type Memory struct {
	mu       sync.Mutex
	sessions map[string]model.Session
	photos   map[string]map[string]model.Photo // sessionID -> photoID -> photo
	hashes   map[string]map[string]string      // sessionID -> hash -> photoID
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		sessions: map[string]model.Session{},
		photos:   map[string]map[string]model.Photo{},
		hashes:   map[string]map[string]string{},
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
