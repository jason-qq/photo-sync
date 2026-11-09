// Package apiclient is a Go client for the photo review API, used by the
// uploader CLI. It speaks the wire contract defined in internal/api.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"photo-sync/internal/api"
)

// APIError is a non-2xx response from the API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api: %d %s: %s", e.StatusCode, http.StatusText(e.StatusCode), e.Message)
}

// Client calls the API as the owner.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a Client for the API at baseURL (e.g. "http://localhost:8080").
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

// CreateSession creates a review session.
func (c *Client) CreateSession(ctx context.Context, req api.CreateSessionRequest) (api.Session, error) {
	var s api.Session
	_, err := c.do(ctx, http.MethodPost, api.SessionsPath(), req, &s)
	return s, err
}

// RegisterPhoto registers a raw file in a session. created reports whether
// it is a new photo (201) rather than an existing one with the same hash
// (200). The response carries UploadURLs whenever renditions should be
// uploaded.
func (c *Client) RegisterPhoto(ctx context.Context, sessionID string, req api.CreatePhotoRequest, force bool) (resp api.PhotoResponse, created bool, err error) {
	path := api.PhotosPath(sessionID)
	if force {
		path += "?" + api.QueryForce + "=true"
	}
	status, err := c.do(ctx, http.MethodPost, path, req, &resp)
	return resp, status == http.StatusCreated, err
}

// CompletePhoto confirms both renditions were uploaded.
func (c *Client) CompletePhoto(ctx context.Context, sessionID, photoID string) (api.PhotoResponse, error) {
	var resp api.PhotoResponse
	_, err := c.do(ctx, http.MethodPost, api.CompletePath(sessionID, photoID), nil, &resp)
	return resp, err
}

// Upload PUTs a JPEG to a presigned upload URL. The API key is NOT sent:
// the URL itself is the credential, and it may point at S3, not the API.
func (c *Client) Upload(ctx context.Context, uploadURL string, jpeg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(jpeg))
	if err != nil {
		return errors.New("upload: invalid upload URL")
	}
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", stripURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// do sends an authenticated JSON request. On 2xx it decodes the response
// into out (if non-nil); otherwise it returns an *APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		var e api.Error
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e) != nil || e.Error == "" {
			e.Error = "unexpected response"
		}
		return resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// stripURL drops the URL from net/http errors, which would otherwise embed
// the presigned URL — and its signature — in logs and error messages.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
