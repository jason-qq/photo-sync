package review

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"photo-sync/internal/model"
	"photo-sync/internal/store"
)

// MaxCommentLen bounds feedback comments, in characters (not bytes, so
// Japanese gets the same allowance as English).
const MaxCommentLen = 2000

// ErrInvalidFeedback is a client error (bad input), wrapped with detail.
var ErrInvalidFeedback = errors.New("invalid feedback")

// SaveFeedback applies a partial update to r's feedback on a photo of
// their session (DESIGN.md §7) and returns the result. Feedback left with
// no fields set is deleted, so "reviewed" always means some feedback
// exists. Only ready photos can get feedback.
func (s *Service) SaveFeedback(ctx context.Context, r model.Reviewer, photoID string, patch model.FeedbackPatch) (model.Feedback, error) {
	if err := checkPatch(patch); err != nil {
		return model.Feedback{}, err
	}
	p, err := s.store.GetPhoto(ctx, r.SessionID, photoID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.Status != model.StatusReady) {
		return model.Feedback{}, ErrPhotoNotFound
	}
	if err != nil {
		return model.Feedback{}, fmt.Errorf("get photo: %w", err)
	}

	f, err := s.store.UpdateFeedback(ctx, r.SessionID, r.ID, photoID, patch, s.now().UTC())
	if err != nil {
		return model.Feedback{}, fmt.Errorf("update feedback: %w", err)
	}
	if f.Empty() {
		if err := s.store.DeleteFeedback(ctx, r.SessionID, r.ID, photoID); err != nil {
			return model.Feedback{}, fmt.Errorf("delete empty feedback: %w", err)
		}
	}
	return f, nil
}

// DeleteFeedback removes all of r's feedback on a photo ("undo"). It is
// idempotent.
func (s *Service) DeleteFeedback(ctx context.Context, r model.Reviewer, photoID string) error {
	if err := s.store.DeleteFeedback(ctx, r.SessionID, r.ID, photoID); err != nil {
		return fmt.Errorf("delete feedback: %w", err)
	}
	return nil
}

// checkPatch enforces the feedback rules: verdict accept/reject, stars
// 1–5, comment within MaxCommentLen; zero values clear. At least one field
// must be present.
func checkPatch(p model.FeedbackPatch) error {
	if p.Verdict == nil && p.Stars == nil && p.Comment == nil {
		return fmt.Errorf("%w: no fields to update", ErrInvalidFeedback)
	}
	if v := p.Verdict; v != nil && *v != model.VerdictNone && *v != model.VerdictAccept && *v != model.VerdictReject {
		return fmt.Errorf("%w: verdict must be accept, reject or null", ErrInvalidFeedback)
	}
	if n := p.Stars; n != nil && (*n < 0 || *n > 5) {
		return fmt.Errorf("%w: stars must be 1-5 or null", ErrInvalidFeedback)
	}
	if c := p.Comment; c != nil {
		if !utf8.ValidString(*c) || utf8.RuneCountInString(*c) > MaxCommentLen {
			return fmt.Errorf("%w: comment must be at most %d characters", ErrInvalidFeedback, MaxCommentLen)
		}
	}
	return nil
}
