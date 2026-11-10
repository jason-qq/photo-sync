package lambdahttp

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func event(method, path, query string) events.APIGatewayV2HTTPRequest {
	ev := events.APIGatewayV2HTTPRequest{
		RawPath:        path,
		RawQueryString: query,
		Headers:        map[string]string{"content-type": "application/json", "sec-fetch-site": "same-origin"},
	}
	ev.RequestContext.DomainName = "abc123.execute-api.us-east-1.amazonaws.com"
	ev.RequestContext.HTTP.Method = method
	ev.RequestContext.HTTP.SourceIP = "203.0.113.7"
	return ev
}

func TestRequestTranslation(t *testing.T) {
	var got *http.Request
	var gotBody string
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))

	ev := event("POST", "/api/sessions/a%20b/photos", "force=true&x=1")
	ev.Cookies = []string{"photosync_owner=v1.123.sig", "other=1"}
	ev.Body = base64.StdEncoding.EncodeToString([]byte(`{"title":"x"}`))
	ev.IsBase64Encoded = true
	if _, err := h(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if got.Method != "POST" || got.URL.Path != "/api/sessions/a b/photos" || got.URL.RawQuery != "force=true&x=1" {
		t.Errorf("method/path/query = %s %q %q", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Host != ev.RequestContext.DomainName || got.RemoteAddr != "203.0.113.7" {
		t.Errorf("host/remote = %q %q", got.Host, got.RemoteAddr)
	}
	if c, err := got.Cookie("photosync_owner"); err != nil || c.Value != "v1.123.sig" {
		t.Errorf("cookie = %v, %v", c, err)
	}
	if got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Sec-Fetch-Site") != "same-origin" {
		t.Errorf("headers = %v", got.Header)
	}
	if gotBody != `{"title":"x"}` || got.ContentLength != int64(len(gotBody)) {
		t.Errorf("body = %q (len %d)", gotBody, got.ContentLength)
	}
}

func TestResponseTranslation(t *testing.T) {
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "a", Value: "1"})
		http.SetCookie(w, &http.Cookie{Name: "b", Value: "2"})
		w.Header().Add("Vary", "Accept")
		w.Header().Add("Vary", "Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	}))
	resp, err := h(context.Background(), event("GET", "/", ""))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || resp.Body != `{"ok":true}` || resp.IsBase64Encoded {
		t.Errorf("status/body = %d %q base64=%v", resp.StatusCode, resp.Body, resp.IsBase64Encoded)
	}
	if !slices.Equal(resp.Cookies, []string{"a=1", "b=2"}) {
		t.Errorf("cookies = %q", resp.Cookies)
	}
	if _, ok := resp.Headers["Set-Cookie"]; ok {
		t.Error("Set-Cookie left in headers")
	}
	if resp.Headers["Vary"] != "Accept, Cookie" {
		t.Errorf("Vary = %q", resp.Headers["Vary"])
	}
}

func TestResponseDefaultsAndBinary(t *testing.T) {
	// No WriteHeader, no Content-Type, no body: 200 with empty text body.
	resp, _ := Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))(context.Background(), event("GET", "/", ""))
	if resp.StatusCode != http.StatusOK || resp.Body != "" || resp.IsBase64Encoded {
		t.Errorf("empty response = %+v", resp)
	}

	bin := []byte{0xff, 0xd8, 0x00, 0x01}
	resp, _ = Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(bin)
	}))(context.Background(), event("GET", "/x.jpg", ""))
	if !resp.IsBase64Encoded || resp.Body != base64.StdEncoding.EncodeToString(bin) {
		t.Errorf("binary response = %+v", resp)
	}

	// Only the first WriteHeader counts, as with net/http.
	resp, _ = Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		w.WriteHeader(http.StatusInternalServerError)
	}))(context.Background(), event("POST", "/", ""))
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestIsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"":                               true, // no Content-Type (e.g. empty 204 body)
		"text/html; charset=utf-8":       true,
		"text/javascript; charset=utf-8": true,
		"application/json":               true,
		"image/svg+xml":                  true,
		"image/jpeg":                     false,
		"application/octet-stream":       false,
		"not a media type;;":             false,
	} {
		if got := isText(ct); got != want {
			t.Errorf("isText(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestBadBase64Body(t *testing.T) {
	ev := event("POST", "/", "")
	ev.Body, ev.IsBase64Encoded = "%%%", true
	if _, err := Handler(http.NotFoundHandler())(context.Background(), ev); err == nil {
		t.Error("bad base64 body accepted")
	}
}
