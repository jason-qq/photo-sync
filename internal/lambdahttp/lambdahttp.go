// Package lambdahttp runs an http.Handler behind API Gateway HTTP APIs
// (payload format 2.0) on AWS Lambda.
//
// API Gateway delivers each request to Lambda as a JSON event and expects a
// JSON response, not HTTP. Handler translates the event into an
// *http.Request, runs the handler in-process, and translates what it wrote
// back. The app itself stays a plain http.Handler, identical to what
// cmd/api serves locally.
//
// Format 2.0 specifics handled here: cookies arrive in a separate field
// rather than the Cookie header, Set-Cookie must be returned in the
// response's cookies field (a headers map can't hold several), and bodies
// that aren't text travel base64-encoded.
package lambdahttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// Handler returns a Lambda handler function serving h.
func Handler(h http.Handler) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return func(ctx context.Context, ev events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		req, err := toRequest(ctx, ev)
		if err != nil {
			// A malformed event is an API Gateway/Lambda wiring problem,
			// not a client error; failing the invocation surfaces it.
			return events.APIGatewayV2HTTPResponse{}, err
		}
		w := &responseWriter{header: http.Header{}}
		h.ServeHTTP(w, req)
		return w.toResponse(), nil
	}
}

func toRequest(ctx context.Context, ev events.APIGatewayV2HTTPRequest) (*http.Request, error) {
	body := []byte(ev.Body)
	if ev.IsBase64Encoded {
		var err error
		if body, err = base64.StdEncoding.DecodeString(ev.Body); err != nil {
			return nil, fmt.Errorf("lambdahttp: decode body: %w", err)
		}
	}

	host := ev.RequestContext.DomainName
	u := "https://" + host + ev.RawPath
	if ev.RawQueryString != "" {
		u += "?" + ev.RawQueryString
	}
	req, err := http.NewRequestWithContext(ctx, ev.RequestContext.HTTP.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("lambdahttp: build request: %w", err)
	}

	// API Gateway joins repeated headers with commas, which is equivalent
	// for everything this app reads.
	for k, v := range ev.Headers {
		req.Header.Set(k, v)
	}
	if len(ev.Cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(ev.Cookies, "; "))
	}
	// Host matters: CrossOriginProtection compares it with Origin.
	req.Host = host
	req.RemoteAddr = ev.RequestContext.HTTP.SourceIP
	req.ContentLength = int64(len(body))
	return req, nil
}

// responseWriter buffers the handler's response in memory. Lambda
// responses are sent whole, so streaming isn't possible anyway.
type responseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

func (w *responseWriter) toResponse() events.APIGatewayV2HTTPResponse {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	resp := events.APIGatewayV2HTTPResponse{
		StatusCode: w.status,
		Headers:    map[string]string{},
		Cookies:    w.header.Values("Set-Cookie"),
	}
	for k, vs := range w.header {
		if k == "Set-Cookie" {
			continue
		}
		resp.Headers[k] = strings.Join(vs, ", ")
	}
	if isText(w.header.Get("Content-Type")) {
		resp.Body = w.body.String()
	} else {
		resp.Body = base64.StdEncoding.EncodeToString(w.body.Bytes())
		resp.IsBase64Encoded = true
	}
	return resp
}

// isText reports whether a response with this Content-Type can be returned
// as a plain string. Empty bodies (no type) are text too.
func isText(contentType string) bool {
	if contentType == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json",
		mt == "application/javascript",
		mt == "application/xml",
		mt == "image/svg+xml":
		return true
	}
	return false
}
