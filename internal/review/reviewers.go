package review

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"photo-sync/internal/model"
	"photo-sync/internal/store"
)

// Reviewer link rules (DESIGN.md §6): one reviewer, one session, an
// expiring secret link the owner can extend or revoke.
const (
	// DefaultReviewerTTL is how long a new link works if no expiry is given.
	DefaultReviewerTTL = 7 * 24 * time.Hour
	// MaxReviewerTTL bounds how far ahead an expiry may be set.
	MaxReviewerTTL = 90 * 24 * time.Hour
	// tokenBytes is the link token's entropy: 256 bits, so guessing one is
	// infeasible and lookups need no rate limiting of their own.
	tokenBytes = 32
	// maxTokenLen rejects absurd input before hashing; real tokens are 43
	// characters.
	maxTokenLen = 128
)

// Reviewer errors. Messages are safe to show to clients.
var (
	ErrReviewerNotFound = errors.New("reviewer not found")
	// ErrReviewerRevoked: a revoked link can't be extended; create a new one.
	ErrReviewerRevoked = errors.New("reviewer link has been revoked")
	// ErrInvalidExpiry is a client error (bad input), wrapped with detail.
	ErrInvalidExpiry = errors.New("invalid expiry")
	// ErrInvalidLink covers unknown, revoked and expired links alike, so a
	// caller learns nothing about which tokens exist.
	ErrInvalidLink = errors.New("this review link is invalid or has expired")
)

// ReviewerLink is a newly created reviewer and its link token. The token is
// returned only here; afterwards only its hash exists.
type ReviewerLink struct {
	Reviewer model.Reviewer
	Token    string
}

// CreateReviewer creates a reviewer for a session with a fresh link token.
// A zero expiresAt means DefaultReviewerTTL from now. The name is stored
// trimmed.
func (s *Service) CreateReviewer(ctx context.Context, sessionID, name string, expiresAt time.Time) (ReviewerLink, error) {
	now := s.now().UTC()
	if expiresAt.IsZero() {
		expiresAt = now.Add(DefaultReviewerTTL)
	}
	if err := checkExpiry(now, expiresAt); err != nil {
		return ReviewerLink{}, err
	}
	token, err := newToken()
	if err != nil {
		return ReviewerLink{}, err
	}
	r := model.Reviewer{
		ID:        model.NewID(),
		SessionID: sessionID,
		Name:      strings.TrimSpace(name),
		TokenHash: hashToken(token),
		ExpiresAt: expiresAt.UTC(),
		CreatedAt: now,
	}
	err = s.store.CreateReviewer(ctx, r)
	if errors.Is(err, store.ErrNotFound) {
		return ReviewerLink{}, ErrSessionNotFound
	}
	if err != nil {
		return ReviewerLink{}, fmt.Errorf("create reviewer: %w", err)
	}
	return ReviewerLink{Reviewer: r, Token: token}, nil
}

// ListReviewers returns a session's reviewers, oldest first.
func (s *Service) ListReviewers(ctx context.Context, sessionID string) ([]model.Reviewer, error) {
	rs, err := s.store.ListReviewers(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("list reviewers: %w", err)
	}
	return rs, nil
}

// ExtendReviewer sets a new expiry, which also revives an expired link.
// Revoked links stay revoked.
func (s *Service) ExtendReviewer(ctx context.Context, sessionID, reviewerID string, expiresAt time.Time) (model.Reviewer, error) {
	if err := checkExpiry(s.now(), expiresAt); err != nil {
		return model.Reviewer{}, err
	}
	r, err := s.getReviewer(ctx, sessionID, reviewerID)
	if err != nil {
		return model.Reviewer{}, err
	}
	if r.Revoked {
		return model.Reviewer{}, ErrReviewerRevoked
	}
	r.ExpiresAt = expiresAt.UTC()
	if err := s.store.SetReviewerExpiry(ctx, sessionID, reviewerID, r.ExpiresAt); err != nil {
		return model.Reviewer{}, fmt.Errorf("extend reviewer: %w", err)
	}
	return r, nil
}

// RevokeReviewer permanently disables a reviewer's link. It is idempotent.
// The reviewer's feedback is kept.
func (s *Service) RevokeReviewer(ctx context.Context, sessionID, reviewerID string) (model.Reviewer, error) {
	r, err := s.getReviewer(ctx, sessionID, reviewerID)
	if err != nil {
		return model.Reviewer{}, err
	}
	if !r.Revoked {
		if err := s.store.RevokeReviewer(ctx, sessionID, reviewerID); err != nil {
			return model.Reviewer{}, fmt.Errorf("revoke reviewer: %w", err)
		}
		r.Revoked = true
	}
	return r, nil
}

// AuthenticateReviewer returns the reviewer a link token belongs to, if the
// link is currently active. Every reviewer request goes through this, so
// revocation and expiry take effect immediately.
func (s *Service) AuthenticateReviewer(ctx context.Context, token string) (model.Reviewer, error) {
	if token == "" || len(token) > maxTokenLen {
		return model.Reviewer{}, ErrInvalidLink
	}
	r, err := s.store.GetReviewerByTokenHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return model.Reviewer{}, ErrInvalidLink
	}
	if err != nil {
		return model.Reviewer{}, fmt.Errorf("look up reviewer: %w", err)
	}
	if !r.Active(s.now()) {
		return model.Reviewer{}, ErrInvalidLink
	}
	return r, nil
}

// ActiveReviewer returns the reviewer named by a reviewer session cookie if
// their link is still active (ErrInvalidLink otherwise). It runs on every
// reviewer request.
func (s *Service) ActiveReviewer(ctx context.Context, sessionID, reviewerID string) (model.Reviewer, error) {
	r, err := s.store.GetReviewer(ctx, sessionID, reviewerID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Reviewer{}, ErrInvalidLink
	}
	if err != nil {
		return model.Reviewer{}, fmt.Errorf("get reviewer: %w", err)
	}
	if !r.Active(s.now()) {
		return model.Reviewer{}, ErrInvalidLink
	}
	return r, nil
}

// ReviewerView is what a reviewer sees: their session, its ready photos
// (filename order, which is shot order) with view URLs, and their own
// feedback.
type ReviewerView struct {
	Session model.Session
	Photos  []PhotoView // Images always set
	// Feedback is this reviewer's feedback by photo ID; photos without
	// feedback have no entry.
	Feedback map[string]model.Feedback
}

// ReviewerView returns the session r reviews, with only ready photos;
// pending ones are hidden from reviewers.
func (s *Service) ReviewerView(ctx context.Context, r model.Reviewer) (ReviewerView, error) {
	sess, err := s.store.GetSession(ctx, r.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return ReviewerView{}, ErrSessionNotFound
	}
	if err != nil {
		return ReviewerView{}, fmt.Errorf("get session: %w", err)
	}
	all, err := s.ListPhotos(ctx, r.SessionID)
	if err != nil {
		return ReviewerView{}, err
	}
	ready := make([]PhotoView, 0, len(all))
	for _, p := range all {
		if p.Images != nil {
			ready = append(ready, p)
		}
	}
	fs, err := s.store.ListFeedback(ctx, r.SessionID, r.ID)
	if err != nil {
		return ReviewerView{}, fmt.Errorf("list feedback: %w", err)
	}
	byPhoto := make(map[string]model.Feedback, len(fs))
	for _, f := range fs {
		byPhoto[f.PhotoID] = f
	}
	return ReviewerView{Session: sess, Photos: ready, Feedback: byPhoto}, nil
}

func (s *Service) getReviewer(ctx context.Context, sessionID, reviewerID string) (model.Reviewer, error) {
	r, err := s.store.GetReviewer(ctx, sessionID, reviewerID)
	if errors.Is(err, store.ErrNotFound) {
		return model.Reviewer{}, ErrReviewerNotFound
	}
	if err != nil {
		return model.Reviewer{}, fmt.Errorf("get reviewer: %w", err)
	}
	return r, nil
}

func checkExpiry(now, expiresAt time.Time) error {
	switch {
	case !expiresAt.After(now):
		return fmt.Errorf("%w: must be in the future", ErrInvalidExpiry)
	case expiresAt.After(now.Add(MaxReviewerTTL)):
		return fmt.Errorf("%w: at most %d days ahead", ErrInvalidExpiry, int(MaxReviewerTTL.Hours()/24))
	}
	return nil
}

// newToken returns a random link token: 32 bytes, base64url without
// padding (43 characters, safe in URLs).
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is how tokens are stored and looked up. A plain SHA-256 is
// enough for 256-bit random tokens (no slow hash needed).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
