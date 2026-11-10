package review

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"photo-sync/internal/model"
)

func ptr[T any](v T) *T { return &v }

// feedbackSetup returns a service, a reviewer and one ready and one pending
// photo in their session.
func feedbackSetup(t *testing.T) (*Service, *clock, model.Reviewer, model.Photo, model.Photo) {
	t.Helper()
	ctx := context.Background()
	svc, clk := newClockedService(t)
	sid := mustSession(t, svc).ID
	ready := readyPhoto(t, svc, svc.blobs.(*fakeBlobs), sid, "a.ARW", hashA)
	pending, _ := svc.RegisterPhoto(ctx, sid, "b.ARW", strings.Repeat("b", 64), false)
	link, _ := svc.CreateReviewer(ctx, sid, "R", time.Time{})
	return svc, clk, link.Reviewer, ready, pending.Photo
}

func TestSaveFeedback(t *testing.T) {
	ctx := context.Background()
	svc, clk, r, ready, _ := feedbackSetup(t)

	f, err := svc.SaveFeedback(ctx, r, ready.ID, model.FeedbackPatch{Verdict: ptr(model.VerdictAccept)})
	if err != nil || f.Verdict != model.VerdictAccept || !f.UpdatedAt.Equal(fixedNow.UTC()) {
		t.Fatalf("save verdict: %+v, %v", f, err)
	}
	clk.t = fixedNow.Add(time.Minute)
	f, _ = svc.SaveFeedback(ctx, r, ready.ID, model.FeedbackPatch{Stars: ptr(5)})
	if f.Verdict != model.VerdictAccept || f.Stars != 5 || !f.UpdatedAt.Equal(clk.t.UTC()) {
		t.Errorf("partial update: %+v", f)
	}

	v, _ := svc.ReviewerView(ctx, r)
	if got := v.Feedback[ready.ID]; got.Verdict != model.VerdictAccept || got.Stars != 5 {
		t.Errorf("view feedback = %+v", v.Feedback)
	}

	// Clearing every field removes the feedback: the photo is unreviewed.
	if _, err := svc.SaveFeedback(ctx, r, ready.ID, model.FeedbackPatch{Verdict: ptr(model.VerdictNone), Stars: ptr(0)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.ReviewerView(ctx, r); len(v.Feedback) != 0 {
		t.Errorf("empty feedback kept: %+v", v.Feedback)
	}
}

func TestSaveFeedbackErrors(t *testing.T) {
	ctx := context.Background()
	svc, _, r, ready, pending := feedbackSetup(t)

	for name, pid := range map[string]string{"unknown photo": "nope", "pending photo": pending.ID} {
		if _, err := svc.SaveFeedback(ctx, r, pid, model.FeedbackPatch{Stars: ptr(3)}); !errors.Is(err, ErrPhotoNotFound) {
			t.Errorf("%s: %v, want ErrPhotoNotFound", name, err)
		}
	}
	// A photo of another session is unknown to this reviewer.
	other := mustSession(t, svc).ID
	otherPhoto := readyPhoto(t, svc, svc.blobs.(*fakeBlobs), other, "x.ARW", strings.Repeat("c", 64))
	if _, err := svc.SaveFeedback(ctx, r, otherPhoto.ID, model.FeedbackPatch{Stars: ptr(3)}); !errors.Is(err, ErrPhotoNotFound) {
		t.Errorf("other session's photo: %v", err)
	}

	bad := map[string]model.FeedbackPatch{
		"empty patch":      {},
		"bad verdict":      {Verdict: ptr(model.Verdict("maybe"))},
		"stars too low":    {Stars: ptr(-1)},
		"stars too high":   {Stars: ptr(6)},
		"comment too long": {Comment: ptr(strings.Repeat("あ", MaxCommentLen+1))},
		"invalid UTF-8":    {Comment: ptr("\xff")},
	}
	for name, p := range bad {
		if _, err := svc.SaveFeedback(ctx, r, ready.ID, p); !errors.Is(err, ErrInvalidFeedback) {
			t.Errorf("%s: %v, want ErrInvalidFeedback", name, err)
		}
	}
	// The limit counts characters: 2000 kana (6000 bytes) is fine.
	if _, err := svc.SaveFeedback(ctx, r, ready.ID, model.FeedbackPatch{Comment: ptr(strings.Repeat("あ", MaxCommentLen))}); err != nil {
		t.Errorf("max-length Japanese comment: %v", err)
	}
}

func TestProgressAndSessionFeedback(t *testing.T) {
	ctx := context.Background()
	svc, clk, alice, p1, _ := feedbackSetup(t)
	sid := alice.SessionID
	p2 := readyPhoto(t, svc, svc.blobs.(*fakeBlobs), sid, "c.ARW", strings.Repeat("c", 64))
	bobLink, _ := svc.CreateReviewer(ctx, sid, "Bob", time.Time{})
	bob := bobLink.Reviewer
	carolLink, _ := svc.CreateReviewer(ctx, sid, "Carol", time.Time{})

	svc.SaveFeedback(ctx, alice, p1.ID, model.FeedbackPatch{Verdict: ptr(model.VerdictAccept)})
	clk.t = fixedNow.Add(time.Hour)
	svc.SaveFeedback(ctx, alice, p2.ID, model.FeedbackPatch{Stars: ptr(3)})
	svc.SaveFeedback(ctx, bob, p1.ID, model.FeedbackPatch{Verdict: ptr(model.VerdictReject)})

	rs, err := svc.ListReviewers(ctx, sid)
	if err != nil || len(rs) != 3 {
		t.Fatalf("ListReviewers: %d, %v", len(rs), err)
	}
	want := map[string]struct {
		n    int
		last time.Time
	}{
		alice.ID:              {2, clk.t.UTC()},
		bob.ID:                {1, clk.t.UTC()},
		carolLink.Reviewer.ID: {0, time.Time{}},
	}
	for _, r := range rs {
		w := want[r.Reviewer.ID]
		if r.Reviewed != w.n || !r.LastActivity.Equal(w.last) {
			t.Errorf("%s: reviewed %d, last %v; want %d, %v", r.Reviewer.Name, r.Reviewed, r.LastActivity, w.n, w.last)
		}
	}

	byPhoto, err := svc.SessionFeedback(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPhoto[p1.ID]) != 2 || len(byPhoto[p2.ID]) != 1 {
		t.Errorf("session feedback = %+v", byPhoto)
	}
}

func TestDeleteFeedback(t *testing.T) {
	ctx := context.Background()
	svc, _, r, ready, _ := feedbackSetup(t)
	svc.SaveFeedback(ctx, r, ready.ID, model.FeedbackPatch{Verdict: ptr(model.VerdictReject), Comment: ptr("x")})
	for range 2 { // idempotent
		if err := svc.DeleteFeedback(ctx, r, ready.ID); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := svc.ReviewerView(ctx, r); len(v.Feedback) != 0 {
		t.Errorf("after delete: %+v", v.Feedback)
	}
}
