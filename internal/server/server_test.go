// Tests are in an external package so they can run the API through the
// same wiring as cmd/api (testserver → app → server) without an import
// cycle.
package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"photo-sync/internal/api"
	"photo-sync/internal/server"
	"photo-sync/internal/testserver"
)

const testKey = testserver.APIKey

// env is the full local app (API + Local blob store) on a test server.
type env struct {
	t   *testing.T
	url string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t, testserver.Start(t).URL}
}

// call sends an authenticated JSON request and decodes the JSON response
// into out (if non-nil). It returns the status code.
func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	return e.do(method, path, testKey, body, out)
}

func (e *env) do(method, path, key string, body any, out any) int {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.url+path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// send sends a request with optional Authorization header and cookie and
// returns the response (body closed).
func (e *env) send(method, path, body, auth string, cookie *http.Cookie) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionCookieFrom(resp *http.Response) *http.Cookie {
	return cookieNamed(resp, server.SessionCookie)
}

func TestBrowserLogin(t *testing.T) {
	e := newEnv(t)

	// Wrong key: 401 and no cookie.
	resp := e.send("POST", "/api/login", `{"apiKey":"wrong"}`, "", nil)
	if resp.StatusCode != http.StatusUnauthorized || sessionCookieFrom(resp) != nil {
		t.Fatalf("wrong key: %d, cookie %v", resp.StatusCode, sessionCookieFrom(resp))
	}
	if code := e.send("POST", "/api/login", `{}`, "", nil).StatusCode; code != http.StatusBadRequest {
		t.Errorf("missing key: %d, want 400", code)
	}

	// Right key: 204 and a hardened cookie.
	resp = e.send("POST", "/api/login", `{"apiKey":"`+testKey+`"}`, "", nil)
	c := sessionCookieFrom(resp)
	if resp.StatusCode != http.StatusNoContent || c == nil {
		t.Fatalf("login: %d, cookie %v", resp.StatusCode, c)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Errorf("cookie attributes: %+v", c)
	}
	if strings.Contains(c.Value, testKey) {
		t.Error("cookie contains the API key")
	}

	// The cookie authorizes owner routes.
	if code := e.send("GET", "/api/sessions", "", "", c).StatusCode; code != http.StatusOK {
		t.Errorf("GET with cookie: %d, want 200", code)
	}
	// A present but wrong Authorization header isn't rescued by the cookie.
	if code := e.send("GET", "/api/sessions", "", "Bearer wrong", c).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("bad bearer + cookie: %d, want 401", code)
	}
	// A tampered cookie is rejected.
	forged := *c
	forged.Value = strings.Replace(c.Value, "v1.", "v1.9", 1)
	if code := e.send("GET", "/api/sessions", "", "", &forged).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("forged cookie: %d, want 401", code)
	}

	// Logout deletes the cookie.
	resp = e.send("POST", "/api/logout", "", "", c)
	if gone := sessionCookieFrom(resp); resp.StatusCode != http.StatusNoContent || gone == nil || gone.MaxAge >= 0 {
		t.Errorf("logout: %d, cookie %+v (want deletion)", resp.StatusCode, gone)
	}
}

func TestReviewerManagement(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	reviewers := api.ReviewersPath(sid)

	// Create: 201, name trimmed, link token in the fragment.
	var created api.CreateReviewerResponse
	if code := e.call("POST", reviewers, map[string]any{"name": "  山田 花子 "}, &created); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	rv := created.Reviewer
	if rv.Name != "山田 花子" || !rv.Active || rv.Revoked || rv.ID == "" {
		t.Errorf("created reviewer: %+v", rv)
	}
	token, ok := strings.CutPrefix(created.LinkPath, "/r#")
	if !ok || len(token) != 43 {
		t.Errorf("linkPath %q: want /r#<43-char token>", created.LinkPath)
	}

	// List: the reviewer, without the token (decoded raw to check that).
	var raw json.RawMessage
	e.call("GET", reviewers, nil, &raw)
	var list api.ListReviewersResponse
	json.Unmarshal(raw, &list)
	if len(list.Reviewers) != 1 || list.Reviewers[0].ID != rv.ID {
		t.Fatalf("list: %s", raw)
	}
	if strings.Contains(string(raw), token) {
		t.Error("list exposes the token")
	}

	// Extend.
	one := api.ReviewerPath(sid, rv.ID)
	newExp := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	var extended api.Reviewer
	if code := e.call("PATCH", one, map[string]any{"expiresAt": newExp}, &extended); code != http.StatusOK || !extended.ExpiresAt.Equal(newExp) {
		t.Errorf("extend: %d %+v", code, extended)
	}

	// Revoke (idempotent), then extending is a conflict.
	for range 2 {
		var revoked api.Reviewer
		if code := e.call("DELETE", one, nil, &revoked); code != http.StatusOK || !revoked.Revoked || revoked.Active {
			t.Errorf("revoke: %d %+v", code, revoked)
		}
	}
	if code := e.call("PATCH", one, map[string]any{"expiresAt": newExp}, nil); code != http.StatusConflict {
		t.Errorf("extend revoked: %d, want 409", code)
	}
}

func TestReviewerValidation(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	reviewers := api.ReviewersPath(sid)
	var created api.CreateReviewerResponse
	e.call("POST", reviewers, map[string]any{"name": "A"}, &created)
	one := api.ReviewerPath(sid, created.Reviewer.ID)

	future := time.Now().Add(24 * time.Hour)
	tests := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"missing name", "POST", reviewers, map[string]any{}, 400},
		{"blank name", "POST", reviewers, map[string]any{"name": "  "}, 400},
		// Limits count characters: 100 kana are 300 bytes but allowed.
		{"100 Japanese chars", "POST", reviewers, map[string]any{"name": strings.Repeat("あ", 100)}, 201},
		{"101 Japanese chars", "POST", reviewers, map[string]any{"name": strings.Repeat("あ", 101)}, 400},
		{"expiry in past", "POST", reviewers, map[string]any{"name": "A", "expiresAt": time.Now().Add(-time.Hour)}, 400},
		{"expiry too far", "POST", reviewers, map[string]any{"name": "A", "expiresAt": time.Now().Add(100 * 24 * time.Hour)}, 400},
		{"explicit expiry", "POST", reviewers, map[string]any{"name": "A", "expiresAt": future}, 201},
		{"unknown session", "POST", api.ReviewersPath("nope"), map[string]any{"name": "A"}, 404},
		{"list unknown session", "GET", api.ReviewersPath("nope"), nil, 404},
		{"extend missing expiry", "PATCH", one, map[string]any{}, 400},
		{"extend unknown", "PATCH", api.ReviewerPath(sid, "nope"), map[string]any{"expiresAt": future}, 404},
		{"revoke unknown", "DELETE", api.ReviewerPath(sid, "nope"), nil, 404},
		{"revoke via other session", "DELETE", api.ReviewerPath(e.createSession(), created.Reviewer.ID), nil, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp map[string]any
			if code := e.call(tt.method, tt.path, tt.body, &resp); code != tt.want {
				t.Errorf("status %d, want %d (%v)", code, tt.want, resp)
			}
		})
	}

	// All four routes require the owner.
	for _, rt := range [][2]string{{"POST", reviewers}, {"GET", reviewers}, {"PATCH", one}, {"DELETE", one}} {
		if code := e.do(rt[0], rt[1], "", map[string]any{"name": "A"}, nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s without key: %d, want 401", rt[0], rt[1], code)
		}
	}
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestReviewerFlow(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := api.PhotosPath(sid)

	// One ready photo, one pending.
	var ready, pending api.PhotoResponse
	e.call("POST", photos, map[string]string{"filename": "SECRET_NAME.ARW", "hash": hashA}, &ready)
	e.upload(ready.UploadURLs.Thumb)
	e.upload(ready.UploadURLs.Preview)
	e.call("POST", api.CompletePath(sid, ready.PhotoID), nil, nil)
	e.call("POST", photos, map[string]string{"filename": "b.ARW", "hash": strings.Repeat("b", 64)}, &pending)

	var created api.CreateReviewerResponse
	e.call("POST", api.ReviewersPath(sid), map[string]any{"name": "山田 花子"}, &created)
	token := strings.TrimPrefix(created.LinkPath, "/r#")

	// Exchange: a wrong token is 403 and sets nothing; the right one sets
	// a hardened reviewer cookie.
	resp := e.send("POST", "/api/review/session", `{"token":"wrong"}`, "", nil)
	if resp.StatusCode != http.StatusForbidden || cookieNamed(resp, server.ReviewerCookie) != nil {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp = e.send("POST", "/api/review/session", `{"token":"`+token+`"}`, "", nil)
	c := cookieNamed(resp, server.ReviewerCookie)
	if resp.StatusCode != http.StatusNoContent || c == nil {
		t.Fatalf("exchange: %d, cookie %v", resp.StatusCode, c)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || strings.Contains(c.Value, token) {
		t.Errorf("cookie: %+v", c)
	}

	// No or forged cookie: 401.
	if code := e.send("GET", "/api/review", "", "", nil).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d, want 401", code)
	}
	forged := *c
	forged.Value = strings.Replace(c.Value, "v1.", "v1.9", 1)
	if code := e.send("GET", "/api/review", "", "", &forged).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("forged cookie: %d, want 401", code)
	}

	// The view: ready photos only, no filenames or hashes, not cacheable.
	req, _ := http.NewRequest("GET", e.url+"/api/review", nil)
	req.AddCookie(c)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("view: %d, Cache-Control %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
	var view api.ReviewerViewResponse
	json.Unmarshal(raw, &view)
	if view.Session.Title != "Shoot" || view.Reviewer.Name != "山田 花子" {
		t.Errorf("view session/reviewer: %+v %+v", view.Session, view.Reviewer)
	}
	if len(view.Photos) != 1 || view.Photos[0].ID != ready.PhotoID {
		t.Fatalf("view photos: %+v", view.Photos)
	}
	for _, leak := range []string{"SECRET_NAME", hashA, "b.ARW", pending.PhotoID} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("view leaks %q", leak)
		}
	}
	if img, err := http.Get(view.Photos[0].Images.Preview); err != nil || img.StatusCode != http.StatusOK {
		t.Errorf("preview URL: %v %v", img, err)
	}

	// Cookies don't cross roles.
	if code := e.send("GET", "/api/sessions", "", "", c).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("reviewer cookie on owner route: %d, want 401", code)
	}
	owner := sessionCookieFrom(e.send("POST", "/api/login", `{"apiKey":"`+testKey+`"}`, "", nil))
	if code := e.send("GET", "/api/review", "", "", owner).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("owner cookie on reviewer route: %d, want 401", code)
	}

	// Revoking ends access on the very next request, cookie or not.
	e.call("DELETE", api.ReviewerPath(sid, created.Reviewer.ID), nil, nil)
	if code := e.send("GET", "/api/review", "", "", c).StatusCode; code != http.StatusForbidden {
		t.Errorf("after revoke: %d, want 403", code)
	}
	if code := e.send("POST", "/api/review/session", `{"token":"`+token+`"}`, "", nil).StatusCode; code != http.StatusForbidden {
		t.Errorf("exchange after revoke: %d, want 403", code)
	}
}

// readyPhoto registers, uploads and completes a photo, returning its ID.
func (e *env) readyPhoto(sid, filename, hash string) string {
	e.t.Helper()
	var p api.PhotoResponse
	e.call("POST", api.PhotosPath(sid), map[string]string{"filename": filename, "hash": hash}, &p)
	e.upload(p.UploadURLs.Thumb)
	e.upload(p.UploadURLs.Preview)
	if code := e.call("POST", api.CompletePath(sid, p.PhotoID), nil, nil); code != http.StatusOK {
		e.t.Fatalf("complete: %d", code)
	}
	return p.PhotoID
}

// reviewerCookie creates a reviewer link for the session and exchanges it
// for a reviewer session cookie.
func (e *env) reviewerCookie(sid, name string) *http.Cookie {
	e.t.Helper()
	var created api.CreateReviewerResponse
	e.call("POST", api.ReviewersPath(sid), map[string]any{"name": name}, &created)
	token := strings.TrimPrefix(created.LinkPath, "/r#")
	c := cookieNamed(e.send("POST", "/api/review/session", `{"token":"`+token+`"}`, "", nil), server.ReviewerCookie)
	if c == nil {
		e.t.Fatal("no reviewer cookie")
	}
	return c
}

// asReviewer sends a JSON request with a reviewer cookie, decodes the
// response into out (if non-nil and there is a body) and returns the
// status.
func (e *env) asReviewer(c *http.Cookie, method, path string, body any, out any) int {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rdr = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.url+path, rdr)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestFeedbackFlow(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	pid := e.readyPhoto(sid, "a.ARW", hashA)
	c := e.reviewerCookie(sid, "A")
	fb := api.FeedbackPath(pid)

	var f api.Feedback
	if code := e.asReviewer(c, "PATCH", fb, `{"verdict":"accept"}`, &f); code != http.StatusOK ||
		f.Verdict == nil || *f.Verdict != "accept" || f.Stars != nil || f.UpdatedAt == nil {
		t.Fatalf("set verdict: %d %+v", code, f)
	}
	// Other fields: the verdict stays.
	f = api.Feedback{}
	e.asReviewer(c, "PATCH", fb, map[string]any{"stars": 4, "comment": "色味が良い\n2行目"}, &f)
	if f.Verdict == nil || *f.Verdict != "accept" || f.Stars == nil || *f.Stars != 4 || f.Comment == nil || *f.Comment != "色味が良い\n2行目" {
		t.Fatalf("set stars+comment: %+v", f)
	}
	// null clears just that field.
	f = api.Feedback{}
	e.asReviewer(c, "PATCH", fb, `{"verdict":null}`, &f)
	if f.Verdict != nil || f.Stars == nil || f.Comment == nil {
		t.Fatalf("clear verdict: %+v", f)
	}

	// The view carries it.
	var view api.ReviewerViewResponse
	e.asReviewer(c, "GET", "/api/review", nil, &view)
	if got := view.Photos[0].Feedback; got == nil || got.Stars == nil || *got.Stars != 4 || got.Verdict != nil {
		t.Errorf("view feedback: %+v", got)
	}

	// Another reviewer of the same session sees none of it.
	other := e.reviewerCookie(sid, "B")
	var otherView api.ReviewerViewResponse
	e.asReviewer(other, "GET", "/api/review", nil, &otherView)
	if otherView.Photos[0].Feedback != nil {
		t.Errorf("reviewer B sees A's feedback: %+v", otherView.Photos[0].Feedback)
	}

	// Delete (idempotent), then the photo is unreviewed.
	for range 2 {
		if code := e.asReviewer(c, "DELETE", fb, nil, nil); code != http.StatusNoContent {
			t.Errorf("delete: %d, want 204", code)
		}
	}
	view = api.ReviewerViewResponse{}
	e.asReviewer(c, "GET", "/api/review", nil, &view)
	if view.Photos[0].Feedback != nil {
		t.Errorf("after delete: %+v", view.Photos[0].Feedback)
	}
}

func TestFeedbackValidation(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	pid := e.readyPhoto(sid, "a.ARW", hashA)
	var pending api.PhotoResponse
	e.call("POST", api.PhotosPath(sid), map[string]string{"filename": "b.ARW", "hash": strings.Repeat("b", 64)}, &pending)
	otherPhoto := e.readyPhoto(e.createSession(), "c.ARW", strings.Repeat("c", 64))
	c := e.reviewerCookie(sid, "A")
	fb := api.FeedbackPath(pid)

	tests := []struct {
		name, path string
		body       any
		want       int
	}{
		{"no fields", fb, `{}`, 400},
		{"bad verdict", fb, `{"verdict":"maybe"}`, 400},
		{"zero stars", fb, `{"stars":0}`, 400},
		{"six stars", fb, `{"stars":6}`, 400},
		{"stars as string", fb, `{"stars":"5"}`, 400},
		{"unknown field", fb, `{"stars":3,"rating":3}`, 400},
		{"2000-char Japanese comment", fb, map[string]string{"comment": strings.Repeat("あ", 2000)}, 200},
		{"2001-char comment", fb, map[string]string{"comment": strings.Repeat("あ", 2001)}, 400},
		{"empty comment clears", fb, `{"comment":""}`, 200},
		{"pending photo", api.FeedbackPath(pending.PhotoID), `{"stars":3}`, 404},
		{"unknown photo", api.FeedbackPath("nope"), `{"stars":3}`, 404},
		{"other session's photo", api.FeedbackPath(otherPhoto), `{"stars":3}`, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp map[string]any
			if code := e.asReviewer(c, "PATCH", tt.path, tt.body, &resp); code != tt.want {
				t.Errorf("status %d, want %d (%v)", code, tt.want, resp)
			}
		})
	}

	// Reviewer routes need the reviewer cookie; the owner key doesn't count.
	for _, method := range []string{"PATCH", "DELETE"} {
		if code := e.call(method, fb, `{"stars":3}`, nil); code != http.StatusUnauthorized {
			t.Errorf("%s with owner key: %d, want 401", method, code)
		}
	}
}

func (e *env) upload(url string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("jpeg"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("upload: status %d", resp.StatusCode)
	}
}

func (e *env) createSession() string {
	e.t.Helper()
	var s struct{ ID string }
	if code := e.call("POST", "/api/sessions", map[string]string{"title": "Shoot", "date": "2026-11-09"}, &s); code != http.StatusCreated {
		e.t.Fatalf("create session: %d", code)
	}
	return s.ID
}

var hashA = strings.Repeat("a", 64)

func TestUploadFlow(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := "/api/sessions/" + sid + "/photos"

	// New photo: 201 with upload URLs.
	var p api.PhotoResponse
	if code := e.call("POST", photos, map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &p); code != http.StatusCreated {
		t.Fatalf("create photo: %d", code)
	}
	if p.PhotoID == "" || p.Status != "pending" || p.UploadURLs == nil {
		t.Fatalf("unexpected response: %+v", p)
	}
	complete := photos + "/" + p.PhotoID + "/complete"

	// Complete before uploading: 409.
	if code := e.call("POST", complete, nil, nil); code != http.StatusConflict {
		t.Fatalf("complete before upload: %d, want 409", code)
	}
	e.upload(p.UploadURLs.Thumb)
	if code := e.call("POST", complete, nil, nil); code != http.StatusConflict {
		t.Fatalf("complete with only thumb: %d, want 409", code)
	}
	e.upload(p.UploadURLs.Preview)

	// Complete: 200 ready, and idempotent.
	for range 2 {
		var done api.PhotoResponse
		if code := e.call("POST", complete, nil, &done); code != http.StatusOK || done.Status != "ready" {
			t.Fatalf("complete: %d %+v", code, done)
		}
	}

	// Same hash again: 200, same photo, no upload URLs.
	var dup api.PhotoResponse
	if code := e.call("POST", photos, map[string]string{"filename": "renamed.ARW", "hash": hashA}, &dup); code != http.StatusOK {
		t.Fatalf("duplicate: %d, want 200", code)
	}
	if dup.PhotoID != p.PhotoID || dup.Status != "ready" || dup.UploadURLs != nil {
		t.Fatalf("duplicate response: %+v", dup)
	}

	// force=true: 200, same photo, fresh upload URLs.
	var forced api.PhotoResponse
	if code := e.call("POST", photos+"?force=true", map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &forced); code != http.StatusOK {
		t.Fatalf("force: %d, want 200", code)
	}
	if forced.PhotoID != p.PhotoID || forced.UploadURLs == nil {
		t.Fatalf("force response: %+v", forced)
	}

	// Same hash in another session: a new photo.
	var other api.PhotoResponse
	otherPhotos := "/api/sessions/" + e.createSession() + "/photos"
	if code := e.call("POST", otherPhotos, map[string]string{"filename": "DSC0001.ARW", "hash": hashA}, &other); code != http.StatusCreated {
		t.Fatalf("other session: %d, want 201", code)
	}
	if other.PhotoID == p.PhotoID {
		t.Fatal("other session reused photo ID")
	}
}

func TestListSessions(t *testing.T) {
	e := newEnv(t)

	var raw map[string]json.RawMessage
	if code := e.call("GET", "/api/sessions", nil, &raw); code != http.StatusOK {
		t.Fatalf("list empty: %d", code)
	}
	if got := string(raw["sessions"]); got != "[]" {
		t.Errorf("empty sessions = %s, want []", got)
	}

	sid := e.createSession()
	var list api.ListSessionsResponse
	if code := e.call("GET", "/api/sessions", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != sid || list.Sessions[0].Title != "Shoot" {
		t.Errorf("sessions = %+v", list.Sessions)
	}
}

func TestListPhotos(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := "/api/sessions/" + sid + "/photos"

	// Empty session: an empty array, not null.
	var raw map[string]json.RawMessage
	if code := e.call("GET", photos, nil, &raw); code != http.StatusOK {
		t.Fatalf("list empty: %d", code)
	}
	if got := string(raw["photos"]); got != "[]" {
		t.Errorf("empty photos = %s, want []", got)
	}

	// One ready photo, one pending.
	var ready, pending api.PhotoResponse
	e.call("POST", photos, map[string]string{"filename": "b.ARW", "hash": hashA}, &ready)
	e.upload(ready.UploadURLs.Thumb)
	e.upload(ready.UploadURLs.Preview)
	if code := e.call("POST", photos+"/"+ready.PhotoID+"/complete", nil, nil); code != http.StatusOK {
		t.Fatalf("complete: %d", code)
	}
	e.call("POST", photos, map[string]string{"filename": "a.ARW", "hash": strings.Repeat("b", 64)}, &pending)

	var list api.ListPhotosResponse
	if code := e.call("GET", photos, nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if len(list.Photos) != 2 {
		t.Fatalf("got %d photos, want 2", len(list.Photos))
	}
	a, b := list.Photos[0], list.Photos[1] // sorted by filename
	if a.ID != pending.PhotoID || a.Status != api.PhotoPending || a.Images != nil {
		t.Errorf("pending photo: %+v", a)
	}
	if b.ID != ready.PhotoID || b.Filename != "b.ARW" || b.Hash != hashA || b.Status != api.PhotoReady || b.Images == nil {
		t.Fatalf("ready photo: %+v", b)
	}

	// The image URLs serve what was uploaded, without the API key.
	for _, url := range []string{b.Images.Thumb, b.Images.Preview} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "jpeg" {
			t.Errorf("GET image: %d %q", resp.StatusCode, body)
		}
	}

	var errResp api.Error
	if code := e.call("GET", "/api/sessions/nope/photos", nil, &errResp); code != http.StatusNotFound || errResp.Error == "" {
		t.Errorf("unknown session: %d %+v, want 404", code, errResp)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	body := map[string]string{"title": "x", "date": "2026-11-09"}
	for _, key := range []string{"", "wrong-key"} {
		if code := e.do("POST", "/api/sessions", key, body, nil); code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", key, code)
		}
		if code := e.do("GET", "/api/sessions", key, nil, nil); code != http.StatusUnauthorized {
			t.Errorf("list sessions, key %q: status %d, want 401", key, code)
		}
		if code := e.do("GET", "/api/sessions/any/photos", key, nil, nil); code != http.StatusUnauthorized {
			t.Errorf("list photos, key %q: status %d, want 401", key, code)
		}
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	sid := e.createSession()
	photos := "/api/sessions/" + sid + "/photos"

	tests := []struct {
		name string
		path string
		body any
		want int
	}{
		{"session missing title", "/api/sessions", map[string]string{"date": "2026-11-09"}, 400},
		{"session blank title", "/api/sessions", map[string]string{"title": "  ", "date": "2026-11-09"}, 400},
		{"session bad date", "/api/sessions", map[string]string{"title": "x", "date": "09/11/2026"}, 400},
		{"unknown field", "/api/sessions", map[string]string{"title": "x", "date": "2026-11-09", "extra": "1"}, 400},
		{"malformed JSON", "/api/sessions", `{"title":`, 400},
		{"trailing data", "/api/sessions", `{"title":"x","date":"2026-11-09"}{}`, 400},
		{"photo bad hash", photos, map[string]string{"filename": "a.ARW", "hash": "abc"}, 400},
		{"photo uppercase hash", photos, map[string]string{"filename": "a.ARW", "hash": strings.Repeat("A", 64)}, 400},
		{"photo path in filename", photos, map[string]string{"filename": `dir\a.ARW`, "hash": hashA}, 400},
		{"photo missing filename", photos, map[string]string{"hash": hashA}, 400},
		{"photo unknown session", "/api/sessions/nope/photos", map[string]string{"filename": "a.ARW", "hash": hashA}, 404},
		{"complete unknown photo", photos + "/nope/complete", nil, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp struct{ Error string }
			if code := e.call("POST", tt.path, tt.body, &resp); code != tt.want {
				t.Errorf("status %d, want %d (error %q)", code, tt.want, resp.Error)
			}
			if resp.Error == "" {
				t.Error("missing error message")
			}
		})
	}
}
