// Package api is the HTTP client for the pancakestack backend.
// Handles bearer-token auth, auto-refresh, and typed requests/responses.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/auth"
	"github.com/glennbech/pancakestack-cli/internal/config"
)

// Client talks to a pancakestack backend. Wraps http.Client with bearer-token
// injection and one-shot token refresh on 401.
type Client struct {
	BaseURL    string
	Tokens     *auth.Tokens
	http       *http.Client
	tokensPath string
}

// New constructs a Client with tokens loaded from disk. Returns an error if
// the user isn't signed in.
func New(baseURL string) (*Client, error) {
	tokensPath, err := config.CredentialsPath()
	if err != nil {
		return nil, err
	}
	t, err := auth.Load(tokensPath)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("not signed in — run `pancakestack login`")
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Tokens:     t,
		http:       &http.Client{Timeout: 30 * time.Second},
		tokensPath: tokensPath,
	}, nil
}

// UploadURLResponse is the shape returned by POST /upload.
type UploadURLResponse struct {
	CollectionID string `json:"collectionId"`
	UploadURL    string `json:"uploadUrl"`
	Key          string `json:"key"`
	ExpiresIn    int    `json:"expiresIn"`
}

// RequestUploadURL asks the backend for a presigned S3 PUT URL for the
// caller's namespace + collectionId.
func (c *Client) RequestUploadURL(ctx context.Context, collectionID string) (*UploadURLResponse, error) {
	var resp UploadURLResponse
	if err := c.postJSON(ctx, "/upload", map[string]string{"collectionId": collectionID}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StackRequest is the body of POST /jobs.
type StackRequest struct {
	CollectionID string         `json:"collectionId"`
	ScriptID     string         `json:"scriptId,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
	InstanceType string         `json:"instanceType,omitempty"`
}

// StackResponse is what POST /jobs returns.
type StackResponse struct {
	JobID           string         `json:"jobId"`
	RunID           string         `json:"runId"`
	InstanceID      string         `json:"instanceId"`
	Status          string         `json:"status"`
	ScriptID        string         `json:"scriptId"`
	EffectiveParams map[string]any `json:"effectiveParams"`
	InputArchive    string         `json:"inputArchive"`
	OutputPrefix    string         `json:"outputPrefix"`
}

// Stack kicks off a stacking job.
func (c *Client) Stack(ctx context.Context, req StackRequest) (*StackResponse, error) {
	var resp StackResponse
	if err := c.postJSON(ctx, "/jobs", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// postJSON does POST <path> with body marshalled as JSON, adds the bearer
// token, and on 401 tries to refresh once. Decodes the response into `out`.
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	do := func() (*http.Response, error) {
		if err := c.ensureFreshTokens(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Tokens.IDToken)
		return c.http.Do(req)
	}

	resp, err := do()
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	// One retry on 401 in case the token was just about to expire.
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if _, err := auth.Refresh(ctx, c.Tokens); err != nil {
			return fmt.Errorf("session expired — run `pancakestack login`: %w", err)
		}
		if err := auth.Save(c.tokensPath, c.Tokens); err != nil {
			return fmt.Errorf("save refreshed tokens: %w", err)
		}
		resp, err = do()
		if err != nil {
			return fmt.Errorf("POST %s (retry): %w", path, err)
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ensureFreshTokens refreshes tokens proactively if they're near expiry.
// Silently no-ops if tokens are still valid.
func (c *Client) ensureFreshTokens(ctx context.Context) error {
	if !c.Tokens.IsExpired() {
		return nil
	}
	if _, err := auth.Refresh(ctx, c.Tokens); err != nil {
		return fmt.Errorf("token refresh failed — run `pancakestack login`: %w", err)
	}
	return auth.Save(c.tokensPath, c.Tokens)
}

// PutToPresignedURL streams `body` to the presigned S3 PUT URL. The URL
// already carries the auth (SigV4 in query string), so we send no headers
// beyond what content-length auto-adds. Returns on non-2xx.
func (c *Client) PutToPresignedURL(ctx context.Context, presignedURL string, body io.Reader, contentLength int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, body)
	if err != nil {
		return err
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	// Use a client with no timeout for large uploads.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT to S3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 PUT returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
