// Package api is the HTTP client for the pancakestack backend.
// Handles bearer-token auth, auto-refresh, and typed requests/responses.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/auth"
	"github.com/glennbech/pancakestack-cli/internal/config"
)

// ErrNotActivated is returned when the backend rejects a call because the
// authenticated user has not redeemed an invite code yet (HTTP 403 with
// code=UNAPPROVED). The CLI's main() surfaces this as a fixed one-line
// message rather than the raw backend error.
var ErrNotActivated = errors.New("please activate your account in a web browser before using the cli")

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

// MultipartInitResp is the shape returned by POST /upload/init.
type MultipartInitResp struct {
	UploadID string `json:"uploadId,omitempty"`
	Key      string `json:"key,omitempty"`
	PartSize int    `json:"partSize,omitempty"`
	// Backend returns Skipped=true when a file with the same sha256
	// already exists in the collection. Client must not start the
	// multipart flow in that case.
	Skipped     bool   `json:"skipped,omitempty"`
	Reason      string `json:"reason,omitempty"`
	DuplicateOf string `json:"duplicateOf,omitempty"`
}

type multipartInitReq struct {
	CollectionID string `json:"collectionId"`
	Filename     string `json:"filename"`
	ContentType  string `json:"contentType,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	// SizeBytes is the total file size in bytes. Required by the backend
	// as of the storage-quota-enforcement change (pancakestack#7) — used
	// to reject uploads that would exceed the caller's plan cap before
	// the multipart upload is minted.
	SizeBytes int64 `json:"sizeBytes"`
}

// InitMultipartUpload starts a multipart upload for the given collection archive.
// sha256 is the lowercase-hex SHA-256 of the file, or empty to skip client-side
// dedup. Pass a hash for the per-file path where dedup is worth the cost; skip
// it for opaque archives where re-tar rarely produces byte-identical output.
// sizeBytes is required (see multipartInitReq.SizeBytes for why).
func (c *Client) InitMultipartUpload(ctx context.Context, collectionID, filename, contentType, sha256 string, sizeBytes int64) (*MultipartInitResp, error) {
	var resp MultipartInitResp
	body := multipartInitReq{
		CollectionID: collectionID,
		Filename:     filename,
		ContentType:  contentType,
		SHA256:       sha256,
		SizeBytes:    sizeBytes,
	}
	if err := c.postJSON(ctx, "/upload/init", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type multipartPartReq struct {
	UploadID   string `json:"uploadId"`
	Key        string `json:"key"`
	PartNumber int    `json:"partNumber"`
}
type multipartPartResp struct {
	URL string `json:"url"`
}

// SignMultipartPart returns a presigned PUT URL for one part of an in-progress upload.
func (c *Client) SignMultipartPart(ctx context.Context, uploadID, key string, partNumber int) (string, error) {
	var resp multipartPartResp
	body := multipartPartReq{UploadID: uploadID, Key: key, PartNumber: partNumber}
	if err := c.postJSON(ctx, "/upload/part", body, &resp); err != nil {
		return "", err
	}
	return resp.URL, nil
}

// CompletedPart pairs a part number with the ETag S3 returned when it was PUT.
type CompletedPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
	// Size is only populated on results from ListMultipartParts —
	// UploadFileMultipart uses it to fast-forward the progress bar so
	// a resume doesn't display "0 MiB" while it silently accounts for
	// gigabytes S3 already has.
	Size int64 `json:"size,omitempty"`
}

// ErrNoSuchUpload is returned by ListMultipartParts when the backend
// reports the multipart upload no longer exists (aborted, or bucket
// lifecycle expired it). Callers should drop any local state file
// referencing this uploadId and re-init instead of retrying.
var ErrNoSuchUpload = errors.New("multipart upload no longer exists on S3")

type multipartListPartsReq struct {
	UploadID string `json:"uploadId"`
	Key      string `json:"key"`
}
type multipartListPartsResp struct {
	Parts    []CompletedPart `json:"parts"`
	PartSize int             `json:"partSize"`
}

// ListMultipartParts asks the backend which parts S3 has already stored
// for an in-flight upload. Powers the CLI's auto-resume: on a re-run,
// we skip re-uploading anything ListParts reports, then complete with
// the merged set of new + existing parts.
//
// Returns ErrNoSuchUpload when the backend reports 404 (the multipart
// was aborted or S3 lifecycle expired it). Callers should treat that as
// "throw away the state file and start over" rather than a retryable error.
func (c *Client) ListMultipartParts(ctx context.Context, uploadID, key string) ([]CompletedPart, int, error) {
	var resp multipartListPartsResp
	body := multipartListPartsReq{UploadID: uploadID, Key: key}
	if err := c.postJSON(ctx, "/upload/list-parts", body, &resp); err != nil {
		if strings.Contains(err.Error(), "NO_SUCH_UPLOAD") || strings.Contains(err.Error(), "returned 404") {
			return nil, 0, ErrNoSuchUpload
		}
		return nil, 0, err
	}
	return resp.Parts, resp.PartSize, nil
}

type multipartCompleteReq struct {
	UploadID string          `json:"uploadId"`
	Key      string          `json:"key"`
	Parts    []CompletedPart `json:"parts"`
}

// MultipartCompleteResp is the shape returned by POST /upload/complete,
// OR (with Skipped=true) synthesized by UploadFileMultipart when the
// backend rejected /upload/init with a duplicate.
type MultipartCompleteResp struct {
	Key       string `json:"key,omitempty"`
	S3URI     string `json:"s3Uri,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// Skipped=true means no upload happened; a file with this checksum
	// already exists in the collection.
	Skipped     bool   `json:"skipped,omitempty"`
	Reason      string `json:"reason,omitempty"`
	DuplicateOf string `json:"duplicateOf,omitempty"`
}

// CompleteMultipartUpload finalizes a multipart upload. Parts must be sorted
// by PartNumber ascending — the backend rejects out-of-order lists.
func (c *Client) CompleteMultipartUpload(ctx context.Context, uploadID, key string, parts []CompletedPart) (*MultipartCompleteResp, error) {
	var resp MultipartCompleteResp
	body := multipartCompleteReq{UploadID: uploadID, Key: key, Parts: parts}
	if err := c.postJSON(ctx, "/upload/complete", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StackRequest is the body of POST /jobs. The job runs against the whole
// collection prefix by default — optional IncludeFiles narrows it to
// a specific filename allowlist (same list the webapp's Filter Frames
// flow sends). Useful for stacking a subset without deleting the rest.
//
// InstanceType is an admin-privileged override. Under frame-based pricing
// the backend picks the compute tier from the workload shape (frame count +
// drizzle), same as the SaaS webapp does; non-admin overrides are rejected
// server-side. Leave empty to let the backend pick.
type StackRequest struct {
	CollectionID string         `json:"collectionId"`
	ScriptID     string         `json:"scriptId,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
	InstanceType string         `json:"instanceType,omitempty"`
	IncludeFiles []string       `json:"includeFiles,omitempty"`
}

// StackResponse is what POST /jobs returns.
type StackResponse struct {
	JobID           string         `json:"jobId"`
	RunID           string         `json:"runId"`
	InstanceID      string         `json:"instanceId"`
	Status          string         `json:"status"`
	ScriptID        string         `json:"scriptId"`
	EffectiveParams map[string]any `json:"effectiveParams"`
	InputPrefix     string         `json:"inputPrefix"`
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

// JobSummary is one row of GET /jobs. Wire shape mirrors the backend's
// jobSummary in lambda-start-job/jobs_list.go.
type JobSummary struct {
	JobID          string `json:"jobId"`
	RunID          string `json:"runId,omitempty"`
	CollectionID   string `json:"collectionId"`
	ScriptID       string `json:"scriptId,omitempty"`
	InstanceType   string `json:"instanceType,omitempty"`
	State          string `json:"state"`
	Status         string `json:"status"`
	Stage          string `json:"stage,omitempty"`
	StageStartedAt string `json:"stageStartedAt,omitempty"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	OutputPrefix   string `json:"outputPrefix,omitempty"`
	ResultKey      string `json:"resultKey,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ListJobsResponse is the envelope from GET /jobs.
type ListJobsResponse struct {
	Jobs []JobSummary `json:"jobs"`
}

// ListJobs returns the caller's jobs, newest first.
func (c *Client) ListJobs(ctx context.Context) (*ListJobsResponse, error) {
	var resp ListJobsResponse
	if err := c.getJSON(ctx, "/jobs", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// JobDetail is GET /jobs/{id}. Extends the DDB record with a presigned
// download URL when the job succeeded. Fields mirror lambda-start-job/jobs.go
// Job + jobs_list.go jobDetail.
type JobDetail struct {
	JobID          string         `json:"jobId"`
	RunID          string         `json:"runId,omitempty"`
	CollectionID   string         `json:"collectionId"`
	ScriptID       string         `json:"scriptId,omitempty"`
	Params         map[string]any `json:"params,omitempty"`
	InstanceType   string         `json:"instanceType,omitempty"`
	InstanceID     string         `json:"instanceId,omitempty"`
	State          string         `json:"state"`
	Stage          string         `json:"stage,omitempty"`
	StageStartedAt string         `json:"stageStartedAt,omitempty"`
	CreatedAt      string         `json:"createdAt"`
	UpdatedAt      string         `json:"updatedAt"`
	InputPrefix    string         `json:"inputPrefix,omitempty"`
	OutputPrefix   string         `json:"outputPrefix,omitempty"`
	ResultKey      string         `json:"resultKey,omitempty"`
	Error          string         `json:"error,omitempty"`
	DownloadURL    string         `json:"downloadUrl,omitempty"`
	ResultFilename string         `json:"resultFilename,omitempty"`
}

// GetJob fetches one job by id.
func (c *Client) GetJob(ctx context.Context, jobID string) (*JobDetail, error) {
	var resp JobDetail
	if err := c.getJSON(ctx, "/jobs/"+jobID, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CancelJob posts to /jobs/{id}/cancel — flips state to cancelled and
// terminates the EC2 instance (if any is still running). Idempotent from
// the server's POV: cancelling a terminal-state job is a no-op with a
// message in the response body.
func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	// Discard the response body — server returns a small JSON acknowledgement
	// but the caller only cares about success/failure.
	var discard map[string]any
	return c.postJSON(ctx, "/jobs/"+jobID+"/cancel", struct{}{}, &discard)
}

// ArchiveCollectionResp mirrors the backend's 202 payload from
// POST /collections/{id}/archive.
type ArchiveCollectionResp struct {
	CollectionID string `json:"collectionId"`
	Archived     bool   `json:"archived"`
	ArchivedAt   int64  `json:"archivedAt"`
	ArchiveState string `json:"archiveState"`
}

// UnarchiveCollectionResp mirrors the 202 payload from
// POST /collections/{id}/unarchive.
type UnarchiveCollectionResp struct {
	CollectionID string `json:"collectionId"`
	Archived     bool   `json:"archived"`
	ArchiveState string `json:"archiveState"`
}

// ArchiveCollection posts to /collections/{id}/archive with a
// type-to-confirm body. The server requires `confirm` to equal either
// the collectionId or the displayName (case-insensitive) — this guards
// against typos on a destructive-shaped operation. Returns 202 with
// archiveState="archiving"; the archive-op worker Lambda tars FITS to
// Backblaze B2 async, so poll GET /collections to see archiveState flip
// to "archived" (usually within a minute for typical Seestar sizes).
func (c *Client) ArchiveCollection(ctx context.Context, collectionID, confirm string) (*ArchiveCollectionResp, error) {
	body := struct {
		Confirm string `json:"confirm"`
	}{Confirm: confirm}
	var resp ArchiveCollectionResp
	if err := c.postJSON(ctx, "/collections/"+collectionID+"/archive", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UnarchiveCollection posts to /collections/{id}/unarchive. No body
// required. Server gates on available quota — the collection's full
// working size must fit in free quota, otherwise returns
// 402 INSUFFICIENT_STORAGE. On success (202), archiveState flips to
// "unarchiving" and the worker pulls the tar back from B2. Poll
// GET /collections for terminal state (archiveState cleared,
// `archived` attribute removed).
func (c *Client) UnarchiveCollection(ctx context.Context, collectionID string) (*UnarchiveCollectionResp, error) {
	var resp UnarchiveCollectionResp
	if err := c.postJSON(ctx, "/collections/"+collectionID+"/unarchive", struct{}{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// LiveImportHeartbeat posts a keep-alive to the backend so the webapp's
// collection detail page can show a "Live import in progress" banner
// while `pancakestack seestar sync` (or any other live uploader) is
// running. The server treats the collection as live for 15s after the
// most recent heartbeat, so tick at least every 5s. Best-effort: on
// failure the caller logs and moves on, since the sync loop's own work
// is unaffected by the banner being briefly missing.
func (c *Client) LiveImportHeartbeat(ctx context.Context, collectionID, folder string, filesSeen int) error {
	body := struct {
		Folder    string `json:"folder,omitempty"`
		FilesSeen int    `json:"filesSeen,omitempty"`
	}{Folder: folder, FilesSeen: filesSeen}
	return c.postJSON(ctx, "/collections/"+collectionID+"/live-import/heartbeat", body, nil)
}

// CollectionRow is the minimal shape returned by GET /collections for a
// single row — only the fields the CLI's archive/unarchive polling
// needs. The server response has more attributes; we ignore them.
type CollectionRow struct {
	CollectionID string `json:"collectionId"`
	DisplayName  string `json:"displayName"`
	Archived     bool   `json:"archived"`
	ArchivedAt   int64  `json:"archivedAt"`
	ArchiveState string `json:"archiveState"`
	ArchiveError string `json:"archiveError"`
	SizeBytes    int64  `json:"sizeBytes"`
}

// GetCollection fetches a single collection row via the list endpoint
// (server currently has no /collections/{id} GET; the list is small).
// Returns nil, nil if the collection is not in the response.
func (c *Client) GetCollection(ctx context.Context, collectionID string) (*CollectionRow, error) {
	var resp struct {
		Collections []CollectionRow `json:"collections"`
	}
	if err := c.getJSON(ctx, "/collections", nil, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Collections {
		if resp.Collections[i].CollectionID == collectionID {
			return &resp.Collections[i], nil
		}
	}
	return nil, nil
}

// MetricPoint is one time/value sample from GET /jobs/{id}/metrics.
type MetricPoint struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// JobMetrics mirrors the backend's metricsResp in lambda-start-job/metrics.go.
// Series keys today: cpuPct, memPct, netInBps, netOutBps, ebsReadBps,
// ebsWriteBps. Empty series arrays are normal for a job in its first ~60s
// (CWA publishes at 1-min intervals).
type JobMetrics struct {
	InstanceID    string                   `json:"instanceId"`
	StartTime     string                   `json:"startTime"`
	EndTime       string                   `json:"endTime"`
	PeriodSeconds int                      `json:"periodSeconds"`
	Series        map[string][]MetricPoint `json:"series"`
	Note          string                   `json:"note,omitempty"`
}

// GetJobMetrics fetches the CloudWatch time-series panel for a job's
// instance. Doubles as a smoke-test surface for the /jobs/{id}/metrics
// endpoint — hit it after any Lambda deploy that touches auth or the CW
// integration to catch IAM regressions before the browser does.
func (c *Client) GetJobMetrics(ctx context.Context, jobID string) (*JobMetrics, error) {
	var resp JobMetrics
	if err := c.getJSON(ctx, "/jobs/"+jobID+"/metrics", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// LogEvent is one CloudWatch entry from GET /logs/{jobId}.
type LogEvent struct {
	Timestamp int64  `json:"timestamp"`
	Message   string `json:"message"`
}

// LogsResponse mirrors the backend's logsResp in lambda-start-job/logs.go.
// HasStream=false is normal for a freshly-launched job — CWA takes ~30s to
// register the stream; callers should poll until true.
type LogsResponse struct {
	Source            string     `json:"source"`
	HasStream         bool       `json:"hasStream"`
	Events            []LogEvent `json:"events"`
	NextForwardToken  string     `json:"nextForwardToken,omitempty"`
	NextBackwardToken string     `json:"nextBackwardToken,omitempty"`
}

// LogsQuery are the optional query params for GetLogs.
//
//	Source        "job" (app trace, the default) or "cloudinit" (boot log).
//	Limit         1..10000; 0 means "server default".
//	NextToken     forward-paginates from a prior response.
//	StartTime     ms epoch, inclusive lower bound; 0 = unset.
//	EndTime       ms epoch, exclusive upper bound; 0 = unset.
//	StartFromHead nil = server default (true). Set to a pointer to false to
//	              get the *tail* of the stream (last Limit events) — that's
//	              the knob CLI --tail turns.
type LogsQuery struct {
	Source        string
	Limit         int
	NextToken     string
	StartTime     int64
	EndTime       int64
	StartFromHead *bool
}

// AskRequest is the body of POST / on the RAG lambda's function URL.
type AskRequest struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

// AskCitation mirrors the RAG lambda's askCitation.
type AskCitation struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Chapter    string  `json:"chapter,omitempty"`
	Section    string  `json:"section,omitempty"`
	RRF        float32 `json:"rrf"`
	DenseRank  int     `json:"denseRank,omitempty"`
	SparseRank int     `json:"sparseRank,omitempty"`
}

// AskResponse mirrors the RAG lambda's askResponse.
type AskResponse struct {
	Answer    string        `json:"answer"`
	Citations []AskCitation `json:"citations"`
	ElapsedMs int64         `json:"elapsedMs"`
}

// Ask calls the RAG lambda directly at its own function URL — separate
// endpoint from the primary backend because the RAG lambda lives at a
// dedicated Lambda URL (not routed through api.pancakestack.net).
// Reuses this client's token machinery (refresh + bearer header) so the
// caller doesn't reimplement auth.
func (c *Client) Ask(ctx context.Context, ragURL string, req AskRequest) (*AskResponse, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal ask: %w", err)
	}
	if err := c.ensureFreshTokens(ctx); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ragURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.Tokens.IDToken)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", ragURL, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ask returned %d: %s", resp.StatusCode, string(buf))
	}
	var out AskResponse
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode ask response: %w (body: %s)", err, string(buf))
	}
	return &out, nil
}

// GetLogs fetches a page of CloudWatch log events for the given job.
func (c *Client) GetLogs(ctx context.Context, jobID string, q LogsQuery) (*LogsResponse, error) {
	params := map[string]string{}
	if q.Source != "" {
		params["source"] = q.Source
	}
	if q.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", q.Limit)
	}
	if q.NextToken != "" {
		params["nextToken"] = q.NextToken
	}
	if q.StartTime > 0 {
		params["startTime"] = fmt.Sprintf("%d", q.StartTime)
	}
	if q.EndTime > 0 {
		params["endTime"] = fmt.Sprintf("%d", q.EndTime)
	}
	if q.StartFromHead != nil {
		params["startFromHead"] = fmt.Sprintf("%t", *q.StartFromHead)
	}
	var resp LogsResponse
	if err := c.getJSON(ctx, "/logs/"+jobID, params, &resp); err != nil {
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
		if isUnapproved(resp.StatusCode, body) {
			return ErrNotActivated
		}
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// out == nil skips decoding — for endpoints that return 204 or a body
	// the caller doesn't care about (e.g. live-import heartbeat).
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// getJSON does GET <path>?<query>, adds the bearer token, retries once on 401
// after refreshing, and decodes the response into out. Query params are
// URL-encoded; pass nil for none.
func (c *Client) getJSON(ctx context.Context, path string, query map[string]string, out any) error {
	full := c.BaseURL + path
	if len(query) > 0 {
		vals := url.Values{}
		for k, v := range query {
			vals.Set(k, v)
		}
		full += "?" + vals.Encode()
	}

	do := func() (*http.Response, error) {
		if err := c.ensureFreshTokens(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Tokens.IDToken)
		return c.http.Do(req)
	}

	resp, err := do()
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

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
			return fmt.Errorf("GET %s (retry): %w", path, err)
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		if isUnapproved(resp.StatusCode, body) {
			return ErrNotActivated
		}
		if qErr := parseStorageQuotaExceeded(resp.StatusCode, body); qErr != nil {
			return qErr
		}
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// isUnapproved reports whether a backend response is the "invite not yet
// redeemed" gate — HTTP 403 with a JSON body carrying code=UNAPPROVED.
func isUnapproved(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return false
	}
	return e.Code == "UNAPPROVED"
}

// StorageQuotaExceededError is the typed error the API client returns
// when the backend rejects an upload/import with 402 STORAGE_QUOTA_EXCEEDED.
// Numeric fields let CLI commands render a formatted message + a --json
// dump for scripts.
type StorageQuotaExceededError struct {
	Message   string `json:"error"`
	Used      int64  `json:"used"`
	Quota     int64  `json:"quota"`
	Requested int64  `json:"requested"`
	Shortfall int64  `json:"shortfall"`
}

func (e *StorageQuotaExceededError) Error() string { return e.Message }

// parseStorageQuotaExceeded returns nil for anything but the specific
// 402 STORAGE_QUOTA_EXCEEDED shape.
func parseStorageQuotaExceeded(status int, body []byte) *StorageQuotaExceededError {
	if status != http.StatusPaymentRequired {
		return nil
	}
	var e struct {
		Code      string `json:"code"`
		Message   string `json:"error"`
		Used      int64  `json:"used"`
		Quota     int64  `json:"quota"`
		Requested int64  `json:"requested"`
		Shortfall int64  `json:"shortfall"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil
	}
	if e.Code != "STORAGE_QUOTA_EXCEEDED" {
		return nil
	}
	return &StorageQuotaExceededError{
		Message:   e.Message,
		Used:      e.Used,
		Quota:     e.Quota,
		Requested: e.Requested,
		Shortfall: e.Shortfall,
	}
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

// ---- Bulk presign (single-PUT per file) ----
//
// For multi-file uploads (drag-drop, `pancake upload *.fits`) we don't
// need multipart per file — each FITS is ~30MB, single PUT is fine.
// Bulk-presigning cuts N round trips (one per file) down to N/batchSize.

type PresignBulkFile struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType,omitempty"`
	SHA256      string `json:"sha256"`
	// SizeBytes is required by the backend as of the storage-quota-
	// enforcement change (pancakestack#7). Backend sums non-duplicate
	// sizes across the batch and rejects the whole request if it would
	// push the caller past their plan cap.
	SizeBytes int64 `json:"sizeBytes"`
}
type presignBulkReq struct {
	CollectionID string            `json:"collectionId"`
	Files        []PresignBulkFile `json:"files"`
}
type PresignedFile struct {
	Name      string `json:"name"`
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int    `json:"expiresIn,omitempty"`
	// Skipped=true → backend already has this checksum in the
	// collection. Client should NOT PUT bytes for this entry.
	Skipped     bool   `json:"skipped,omitempty"`
	Reason      string `json:"reason,omitempty"`
	DuplicateOf string `json:"duplicateOf,omitempty"`
}
type presignBulkResp struct {
	Files []PresignedFile `json:"files"`
}

// PresignBulk asks the backend to sign single-PUT URLs for up to 500
// files at once. Chunk client-side above 500.
func (c *Client) PresignBulk(ctx context.Context, collectionID string, files []PresignBulkFile) ([]PresignedFile, error) {
	var resp presignBulkResp
	if err := c.postJSON(ctx, "/upload/presign", presignBulkReq{
		CollectionID: collectionID,
		Files:        files,
	}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// BulkUploadOptions drives UploadFilesBulk.
type BulkUploadOptions struct {
	CollectionID string
	// Paths are the local files to upload. Filename preserved as basename.
	Paths []string
	// SHA256s are the lowercase-hex SHA-256 hashes of each Path, in the
	// same order. Required — backend uses these to reject duplicates
	// before returning a presigned URL. Caller hashes upfront so it can
	// report hashing progress separately from upload progress.
	SHA256s []string
	// Concurrency in-flight PUTs. Default 4 — the storage backend handles
	// more but home upstreams choke past that. CLI lets user override.
	Concurrency int
	// OnFileDone fires once per file after successful PUT. May be called
	// from many goroutines. `done`/`total` are file counts, not bytes.
	OnFileDone func(name string, done, total int)
	// OnFileError fires when a file fails. Upload continues; err surfaces
	// aggregated after all files attempt.
	OnFileError func(name string, err error)
	// OnFileSkipped fires when the backend rejected a presign because
	// the file is a duplicate. Not an error — same file already sits in
	// the collection. `duplicateOf` names the existing file.
	OnFileSkipped func(name, reason, duplicateOf string)
}

// BulkUploadResult reports per-file outcome. Successful uploads have
// Error == nil. Skipped=true means the backend deduped this file — no
// PUT was attempted and Error is nil.
type BulkUploadResult struct {
	Name        string
	Key         string
	Error       error
	Skipped     bool
	Reason      string
	DuplicateOf string
}

// presignBulkChunkSize matches backend cap. Chunk paths into batches so
// a single presign call stays snappy.
const presignBulkChunkSize = 500

// UploadFilesBulk uploads N local files as individual S3 objects under
// the collection prefix. Uses /upload/presign for URLs, then plain HTTP
// PUTs with the requested concurrency. Returns per-file results — some
// files can succeed while others fail; caller decides how to summarize.
//
// Files are presigned in batches of 500 to keep any single presign
// request lightweight; uploads pipeline across batches (batch N+1 can
// start signing while batch N is still uploading).
func (c *Client) UploadFilesBulk(ctx context.Context, opts BulkUploadOptions) ([]BulkUploadResult, error) {
	if len(opts.Paths) == 0 {
		return nil, fmt.Errorf("Paths required")
	}
	if len(opts.SHA256s) != len(opts.Paths) {
		return nil, fmt.Errorf("SHA256s must be the same length as Paths (%d vs %d)",
			len(opts.SHA256s), len(opts.Paths))
	}
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}

	// Presign everything upfront in batches. Simpler than pipelining and
	// finishes in a few seconds even for thousands of files. If we outgrow
	// this later, switch to a pipelined signer.
	presigned := make([]PresignedFile, 0, len(opts.Paths))
	pathByName := make(map[string]string, len(opts.Paths))
	for _, p := range opts.Paths {
		name := filepath.Base(p)
		if _, ok := pathByName[name]; ok {
			return nil, fmt.Errorf("duplicate filename in batch: %s", name)
		}
		pathByName[name] = p
	}
	names := make([]PresignBulkFile, 0, len(opts.Paths))
	for i, p := range opts.Paths {
		name := filepath.Base(p)
		ct := contentTypeFor(name)
		// Stat for size — backend requires it for the storage-quota gate.
		// One extra syscall per file; negligible next to the hash we
		// already computed and the S3 PUT we're about to issue.
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
		names = append(names, PresignBulkFile{
			Name:        name,
			ContentType: ct,
			SHA256:      opts.SHA256s[i],
			SizeBytes:   st.Size(),
		})
	}
	for start := 0; start < len(names); start += presignBulkChunkSize {
		end := start + presignBulkChunkSize
		if end > len(names) {
			end = len(names)
		}
		batch, err := c.PresignBulk(ctx, opts.CollectionID, names[start:end])
		if err != nil {
			return nil, fmt.Errorf("presign batch [%d..%d]: %w", start, end, err)
		}
		presigned = append(presigned, batch...)
	}

	total := len(presigned)
	results := make([]BulkUploadResult, total)
	var done int64
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, pf := range presigned {
		// Server-side dedup: nothing to upload. Report and move on;
		// count skips toward the "done" progress so the totals reconcile.
		if pf.Skipped {
			results[i] = BulkUploadResult{
				Name:        pf.Name,
				Skipped:     true,
				Reason:      pf.Reason,
				DuplicateOf: pf.DuplicateOf,
			}
			if opts.OnFileSkipped != nil {
				opts.OnFileSkipped(pf.Name, pf.Reason, pf.DuplicateOf)
			}
			n := atomic.AddInt64(&done, 1)
			if opts.OnFileDone != nil {
				opts.OnFileDone(pf.Name, int(n), total)
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, pf PresignedFile) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = BulkUploadResult{Name: pf.Name, Key: pf.Key, Error: ctx.Err()}
				return
			}
			path := pathByName[pf.Name]
			f, err := os.Open(path)
			if err != nil {
				results[i] = BulkUploadResult{Name: pf.Name, Key: pf.Key, Error: err}
				if opts.OnFileError != nil {
					opts.OnFileError(pf.Name, err)
				}
				return
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				results[i] = BulkUploadResult{Name: pf.Name, Key: pf.Key, Error: err}
				if opts.OnFileError != nil {
					opts.OnFileError(pf.Name, err)
				}
				return
			}
			if err := putPresignedObject(ctx, pf.URL, f, info.Size(), contentTypeFor(pf.Name)); err != nil {
				results[i] = BulkUploadResult{Name: pf.Name, Key: pf.Key, Error: err}
				if opts.OnFileError != nil {
					opts.OnFileError(pf.Name, err)
				}
				return
			}
			results[i] = BulkUploadResult{Name: pf.Name, Key: pf.Key}
			n := atomic.AddInt64(&done, 1)
			if opts.OnFileDone != nil {
				opts.OnFileDone(pf.Name, int(n), total)
			}
		}(i, pf)
	}
	wg.Wait()
	return results, nil
}

// putPresignedObject PUTs a whole file to a presigned URL. Content-Type
// header must match the one baked into the signed URL — the backend
// signs with the same value we compute here.
func putPresignedObject(ctx context.Context, presignedURL string, body io.Reader, length int64, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("storage backend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// contentTypeFor is a small extension → MIME map. Matters because the
// backend bakes Content-Type into the presigned URL — a mismatch on
// PUT returns a 403 SignatureDoesNotMatch.
func contentTypeFor(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".fit"), strings.HasSuffix(lower, ".fits"):
		return "application/fits"
	case strings.HasSuffix(lower, ".fz"):
		return "application/octet-stream"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	case strings.HasSuffix(lower, ".tar"):
		return "application/x-tar"
	}
	return "application/octet-stream"
}

// MultipartUploadOptions drives UploadFileMultipart.
type MultipartUploadOptions struct {
	CollectionID string
	Filename     string
	ContentType  string
	Data         io.ReaderAt
	Size         int64
	// SHA256 of the file contents (lowercase hex). Optional — pass empty
	// to skip client-side dedup. Worth setting for stable per-file uploads
	// where the backend can short-circuit a re-upload; not worth the CPU
	// cost for large opaque archives that hash uniquely each time anyway.
	SHA256      string
	Concurrency int
	// OnProgress fires whenever a part finishes; may be called from many goroutines.
	OnProgress func(uploadedBytes, totalBytes int64)

	// Resume plumbing. When AbsPath + MTimeUnixNano are both set we
	// persist a state file on init and, on the next invocation for the
	// same (collection, filename, path, size, mtime) tuple, call
	// ListMultipartParts to skip re-uploading anything S3 already has.
	// Leave both zero to disable resume (small files, ephemeral data,
	// tests) — the upload works exactly as before.
	AbsPath       string
	MTimeUnixNano int64
	// NoResume forces a fresh multipart even when saved state exists.
	// Rare escape hatch: only useful when a user knows the local file
	// changed underneath our size/mtime check (e.g. same size after an
	// edit, filesystem with 1s mtime granularity).
	NoResume bool
	// OnResume fires once before uploading if we're picking up where a
	// prior invocation left off. Lets the caller print a "resuming from
	// part X/Y (Z MiB already on S3)" message without teaching the api
	// package about stderr formatting.
	OnResume func(alreadyDoneParts, totalParts int, alreadyDoneBytes int64)
}

// UploadFileMultipart orchestrates init → parallel part PUTs → complete for a
// seekable data source. Required for anything above the S3 5 GiB single-PUT
// ceiling; safe (if slightly chattier) for small files too.
//
// Auto-resume: if opts.AbsPath and opts.MTimeUnixNano are set, we persist
// the {uploadID, key, partSize} tuple to disk after init. A subsequent
// call with the same file (same collection, filename, path, size, mtime)
// re-uses the multipart upload — asks the backend which parts S3 already
// has, only re-sends the missing ones, and completes with the merged set.
func (c *Client) UploadFileMultipart(ctx context.Context, opts MultipartUploadOptions) (*MultipartCompleteResp, error) {
	if opts.Data == nil {
		return nil, fmt.Errorf("Data required")
	}
	if opts.Size <= 0 {
		return nil, fmt.Errorf("Size must be > 0")
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	uploadID, key, partSize, existing, err := c.initOrResume(ctx, opts, contentType)
	if err != nil {
		return nil, err
	}
	// Duplicate-shortcut path: initOrResume returned zeroes + a synthesized
	// skipped response is delivered via the sentinel key "__skipped__".
	if key == "__skipped__" {
		return &MultipartCompleteResp{
			Skipped:     true,
			Reason:      existing[0].ETag, // reused as reason carrier
			DuplicateOf: existing[1].ETag,
		}, nil
	}

	total := int((opts.Size + partSize - 1) / partSize)
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}
	if concurrency > total {
		concurrency = total
	}

	// Seed the completed slots from anything ListMultipartParts told us
	// S3 already had, and skip those partNumbers when enqueueing work.
	// Also pre-credit their bytes to the progress bar so a mostly-done
	// resume doesn't render "0.0 MiB" for a minute while the missing
	// tail streams up.
	completed := make([]CompletedPart, total)
	var uploadedBytes int64
	alreadyDone := make(map[int]bool, len(existing))
	var resumedBytes int64
	for _, p := range existing {
		if p.PartNumber < 1 || p.PartNumber > total {
			// S3 has a part we don't recognise. Safer to abort than to
			// complete with a mystery part — could indicate the local
			// file changed size after we started but before our size+mtime
			// check would have caught it. Force a fresh upload.
			return nil, fmt.Errorf("resume: S3 reports part %d but this file only has %d parts — start over with --no-resume", p.PartNumber, total)
		}
		completed[p.PartNumber-1] = CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
		alreadyDone[p.PartNumber] = true
		resumedBytes += p.Size
	}
	if resumedBytes > 0 {
		uploadedBytes = resumedBytes
		if opts.OnResume != nil {
			opts.OnResume(len(existing), total, resumedBytes)
		}
		if opts.OnProgress != nil {
			opts.OnProgress(resumedBytes, opts.Size)
		}
	}

	pending := total - len(alreadyDone)
	if pending == 0 {
		// Everything's on S3 already — user re-ran after the process died
		// between the last part upload and CompleteMultipartUpload.
		// Just complete.
		resp, err := c.CompleteMultipartUpload(ctx, uploadID, key, completed)
		if err != nil {
			return nil, err
		}
		c.clearUploadState(opts)
		return resp, nil
	}

	partCh := make(chan int, pending)
	for i := 1; i <= total; i++ {
		if !alreadyDone[i] {
			partCh <- i
		}
	}
	close(partCh)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		firstEr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstEr = err
			cancel()
		})
	}
	if concurrency > pending {
		concurrency = pending
	}
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for partNumber := range partCh {
				if ctx.Err() != nil {
					return
				}
				offset := int64(partNumber-1) * partSize
				length := partSize
				if offset+length > opts.Size {
					length = opts.Size - offset
				}
				url, err := c.SignMultipartPart(ctx, uploadID, key, partNumber)
				if err != nil {
					fail(fmt.Errorf("sign part %d: %w", partNumber, err))
					return
				}
				section := io.NewSectionReader(opts.Data, offset, length)
				etag, err := putPresignedPart(ctx, url, section, length)
				if err != nil {
					fail(fmt.Errorf("upload part %d: %w", partNumber, err))
					return
				}
				completed[partNumber-1] = CompletedPart{PartNumber: partNumber, ETag: etag}
				n := atomic.AddInt64(&uploadedBytes, length)
				if opts.OnProgress != nil {
					opts.OnProgress(n, opts.Size)
				}
			}
		}()
	}
	wg.Wait()
	if firstEr != nil {
		return nil, firstEr
	}

	resp, err := c.CompleteMultipartUpload(ctx, uploadID, key, completed)
	if err != nil {
		return nil, err
	}
	c.clearUploadState(opts)
	return resp, nil
}

// initOrResume returns the multipart upload we should use for this
// upload attempt — either a fresh one from /upload/init or the existing
// one recorded in a state file on disk. Return shape:
//
//	uploadID, key, partSize — always valid for the caller to stream against.
//	existing               — parts S3 already has (empty for a fresh upload).
//	err                    — surfaced verbatim.
//
// Special case: if /upload/init returns Skipped=true (duplicate), we
// return the sentinel key "__skipped__" and stash reason/duplicateOf in
// the first two elements of `existing`. Ugly but avoids a second return
// path muddying the main flow.
func (c *Client) initOrResume(ctx context.Context, opts MultipartUploadOptions, contentType string) (string, string, int64, []CompletedPart, error) {
	// Resume path — only when the caller told us where the file lives.
	// Without AbsPath + MTimeUnixNano we can't safely key state, so we
	// don't try.
	if !opts.NoResume && opts.AbsPath != "" && opts.MTimeUnixNano != 0 {
		saved, err := LoadUploadState(opts.CollectionID, opts.Filename, opts.AbsPath, opts.Size, opts.MTimeUnixNano)
		if err != nil {
			// Corrupt state file. Delete and start fresh rather than
			// halting — the alternative is a permanent wedge on this
			// specific file until the user manually rms the state.
			_ = DeleteUploadState(opts.CollectionID, opts.Filename, opts.AbsPath, opts.Size, opts.MTimeUnixNano)
			saved = nil
		}
		if saved != nil {
			parts, returnedPartSize, listErr := c.ListMultipartParts(ctx, saved.UploadID, saved.Key)
			if listErr == nil {
				// Priority for partSize:
				//   1. Any uploaded part's actual size (max) — authoritative,
				//      matches what S3 stored + what the original CLI signed
				//      against. Handles the manual-resume case where saved
				//      PartSize is zero because the user seeded state from
				//      just an uploadID + key.
				//   2. saved.PartSize — what we told S3 originally on the
				//      first CLI run.
				//   3. Backend's returned partSize — fallback for the
				//      never-uploaded-a-part-yet case.
				partSize := int64(returnedPartSize)
				if saved.PartSize > 0 {
					partSize = int64(saved.PartSize)
				}
				for _, p := range parts {
					if p.Size > partSize {
						partSize = p.Size
					}
				}
				if partSize <= 0 {
					return "", "", 0, nil, fmt.Errorf("resume: could not determine partSize (empty state, empty S3, empty backend response)")
				}
				return saved.UploadID, saved.Key, partSize, parts, nil
			}
			if errors.Is(listErr, ErrNoSuchUpload) {
				// S3 aborted or expired the multipart. Drop stale state
				// and fall through to fresh init.
				_ = DeleteUploadState(opts.CollectionID, opts.Filename, opts.AbsPath, opts.Size, opts.MTimeUnixNano)
			} else {
				// Some other error (auth, network, backend down). Don't
				// silently start a fresh 100-GB upload — surface the
				// error so the user can retry.
				return "", "", 0, nil, fmt.Errorf("resume: list parts: %w", listErr)
			}
		}
	}

	// Fresh init.
	init, err := c.InitMultipartUpload(ctx, opts.CollectionID, opts.Filename, contentType, opts.SHA256, opts.Size)
	if err != nil {
		return "", "", 0, nil, err
	}
	if init.Skipped {
		return "", "__skipped__", 0, []CompletedPart{
			{ETag: init.Reason},
			{ETag: init.DuplicateOf},
		}, nil
	}
	partSize := int64(init.PartSize)
	if partSize <= 0 {
		return "", "", 0, nil, fmt.Errorf("backend returned zero partSize")
	}
	// S3 caps a multipart upload at 10 000 parts. If the backend's suggested
	// partSize would blow past that for this file, round it up so total ≤ 10 000.
	// Without this the CLI happily asks the backend to sign part 10 001 and
	// gets a 400 partNumber-out-of-range.
	const maxParts = 10000
	if (opts.Size+partSize-1)/partSize > maxParts {
		const mib = 1024 * 1024
		needed := (opts.Size + maxParts - 1) / maxParts
		partSize = ((needed + mib - 1) / mib) * mib
	}

	// Persist state so a crash between here and Complete leaves us with
	// something to resume from. Best-effort — a write failure shouldn't
	// abort the upload; worst case we lose resume for this one file.
	if opts.AbsPath != "" && opts.MTimeUnixNano != 0 {
		_ = SaveUploadState(&UploadState{
			CollectionID:  opts.CollectionID,
			Filename:      opts.Filename,
			AbsPath:       opts.AbsPath,
			Size:          opts.Size,
			MTimeUnixNano: opts.MTimeUnixNano,
			UploadID:      init.UploadID,
			Key:           init.Key,
			PartSize:      int(partSize),
			ContentType:   contentType,
			CreatedAt:     time.Now().UTC(),
		})
	}

	return init.UploadID, init.Key, partSize, nil, nil
}

// clearUploadState best-effort removes the state file after a successful
// complete. Never propagates errors — losing the state file just means
// the next upload of a different file uses one more inode.
func (c *Client) clearUploadState(opts MultipartUploadOptions) {
	if opts.AbsPath == "" || opts.MTimeUnixNano == 0 {
		return
	}
	_ = DeleteUploadState(opts.CollectionID, opts.Filename, opts.AbsPath, opts.Size, opts.MTimeUnixNano)
}

// partUploadMaxAttempts is the total number of times we'll PUT a single
// part before giving up. Set high enough that a residential connection
// dropping mid-upload of one 8 MiB part doesn't kill an 8-hour, 10,000-part
// job — a single broken pipe used to nuke the whole upload (see the
// 2026-09-01 Lobster Nebula incident, 8128 parts in). Not infinite: at
// some point the presigned URL expires (1h backend-side) and the caller
// needs a real error, not a hang.
const partUploadMaxAttempts = 6

// putPresignedPart PUTs a single part and returns the ETag with quotes stripped.
// The URL already carries SigV4 auth in its query string, so we send no
// headers beyond Content-Length. Body must be seekable — we rewind to 0
// before each attempt so retries send the same bytes, not a drained reader.
func putPresignedPart(ctx context.Context, presignedURL string, body io.ReadSeeker, length int64) (string, error) {
	client := &http.Client{}
	var lastErr error
	for attempt := 1; attempt <= partUploadMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("rewind part body: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, body)
		if err != nil {
			return "", err
		}
		req.ContentLength = length
		resp, err := client.Do(req)
		if err != nil {
			// Transport-level error — broken pipe, connection reset,
			// DNS blip. Always retryable unless the ctx is done.
			lastErr = fmt.Errorf("PUT to S3: %w", err)
			if !sleepBackoff(ctx, attempt) {
				return "", lastErr
			}
			continue
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("S3 returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			// 5xx / 408 / 429 are retryable; other 4xx are the caller's
			// fault (bad signature, expired URL, wrong length) — no point
			// hammering S3.
			if !isRetryableStatus(resp.StatusCode) {
				return "", lastErr
			}
			if !sleepBackoff(ctx, attempt) {
				return "", lastErr
			}
			continue
		}
		etag := resp.Header.Get("ETag")
		resp.Body.Close()
		if etag == "" {
			return "", fmt.Errorf("S3 did not return an ETag")
		}
		return strings.Trim(etag, `"`), nil
	}
	return "", lastErr
}

// isRetryableStatus returns true for HTTP statuses where the same request
// might succeed on retry. 5xx = server hiccup. 408 = timeout, request never
// really landed. 429 = told to slow down. Everything else in 4xx is a bug
// in the request (bad sig, wrong length, expired URL) — retrying won't help.
func isRetryableStatus(code int) bool {
	if code >= 500 {
		return true
	}
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
}

// sleepBackoff waits for attempt N's backoff (1s, 2s, 4s, 8s, 16s, capped
// at 30s) with ±25% jitter to avoid thundering-herd retries when a shared
// upstream (S3 partition, home router) drops many parallel workers at
// once. Returns false if ctx completes during the sleep; true otherwise
// so the caller keeps looping. Doesn't sleep after the final attempt —
// the caller checks the loop counter.
func sleepBackoff(ctx context.Context, attempt int) bool {
	if attempt >= partUploadMaxAttempts {
		return false
	}
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	// Deterministic jitter — attempt count as the only seed input keeps
	// this testable without hauling in math/rand. Range is base*[0.75, 1.25].
	jitter := time.Duration(int64(base) * int64(attempt*7%50-25) / 100)
	d := base + jitter
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// ---- Bulk download presign ----
//
// Symmetric with /upload/presign: caller supplies filenames, backend
// returns one presigned single-GET URL per file. Client streams downloads
// with concurrency. Names come from ListCollectionFiles (which also
// carries sizes for progress + resume).

// CollectionFile is the trimmed subset of the backend's collectionFile
// wire shape that the CLI needs for listing + download reconciliation.
// The full shape has ~50 FITS-metadata fields the CLI doesn't consume.
type CollectionFile struct {
	Name         string `json:"name"`
	SizeBytes    int64  `json:"sizeBytes"`
	LastModified string `json:"lastModified"`
	Kind         string `json:"kind"`
}

type listCollectionFilesResp struct {
	Files     []CollectionFile `json:"files"`
	NextToken string           `json:"nextToken,omitempty"`
	Source    string           `json:"source"`
}

// ListCollectionFiles walks every page of GET /collections/{id}/files and
// returns the aggregated list. Backend caps pageSize at 1000; we let it
// pick the default (currently 500) and follow nextToken until exhausted.
func (c *Client) ListCollectionFiles(ctx context.Context, collectionID string) ([]CollectionFile, error) {
	var all []CollectionFile
	nextToken := ""
	for {
		q := map[string]string{}
		if nextToken != "" {
			q["nextToken"] = nextToken
		}
		var page listCollectionFilesResp
		if err := c.getJSON(ctx, "/collections/"+collectionID+"/files", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.NextToken == "" {
			break
		}
		nextToken = page.NextToken
	}
	return all, nil
}

type presignDownloadReq struct {
	Names []string `json:"names"`
}

// PresignedDownload is one signed GET URL from PresignDownloadBulk.
type PresignedDownload struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	URL       string `json:"url"`
	ExpiresIn int    `json:"expiresIn"`
}

type presignDownloadResp struct {
	Files []PresignedDownload `json:"files"`
}

// PresignDownloadBulk asks the backend to sign single-GET URLs for up to
// 500 filenames at once. Chunk client-side above 500.
func (c *Client) PresignDownloadBulk(ctx context.Context, collectionID string, names []string) ([]PresignedDownload, error) {
	var resp presignDownloadResp
	if err := c.postJSON(ctx, "/collections/"+collectionID+"/download/presign",
		presignDownloadReq{Names: names}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// BulkDownloadOptions drives DownloadFilesBulk.
type BulkDownloadOptions struct {
	CollectionID string
	// Files are what to download — name + expected size. Size is used to
	// skip files already present locally (same size = assume same content;
	// good enough for FITS which are effectively immutable once captured).
	Files []CollectionFile
	// DestDir is the local directory to write into. Created if absent.
	DestDir string
	// Concurrency in-flight GETs. Default 4.
	Concurrency int
	// SkipExisting=true skips writing a file if a same-size local file
	// exists at DestDir/name. Default true — callers rarely want to re-
	// download on a resume.
	SkipExisting bool
	// OnFileDone fires once per file that finishes (download OR skip).
	// May be called from many goroutines.
	OnFileDone func(name string, sizeBytes int64, skipped bool, done, total int)
	// OnFileError fires when a file fails. Download continues; the error
	// is aggregated into the per-file result.
	OnFileError func(name string, err error)
}

// BulkDownloadResult reports per-file outcome. Successful downloads have
// Error == nil. Skipped=true means the file was already on disk at the
// expected size; no GET was attempted.
type BulkDownloadResult struct {
	Name    string
	Path    string
	Skipped bool
	Error   error
}

// presignDownloadChunkSize matches backend cap.
const presignDownloadChunkSize = 500

// DownloadFilesBulk downloads N collection files to DestDir. Uses
// /collections/{id}/download/presign for URLs (batches of 500), then
// plain HTTP GETs with the requested concurrency. Returns per-file
// results — some can succeed while others fail; caller summarizes.
//
// Resumable: SkipExisting=true (the default) skips files whose local
// size matches the collection metadata. Kill and re-run to pick up
// where a prior invocation stopped.
func (c *Client) DownloadFilesBulk(ctx context.Context, opts BulkDownloadOptions) ([]BulkDownloadResult, error) {
	if len(opts.Files) == 0 {
		return nil, fmt.Errorf("Files required")
	}
	if opts.DestDir == "" {
		return nil, fmt.Errorf("DestDir required")
	}
	if err := os.MkdirAll(opts.DestDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", opts.DestDir, err)
	}
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}
	// SkipExisting defaults on — callers opt out by passing an explicit
	// false via a separate flag if we ever need it.
	skipExisting := opts.SkipExisting

	total := len(opts.Files)
	results := make([]BulkDownloadResult, total)
	byName := make(map[string]int, total)
	for i, f := range opts.Files {
		byName[f.Name] = i
	}

	// Partition into (skip-locally-present, need-to-download). Skips
	// count toward "done" so the progress totals reconcile.
	var toFetch []string
	var doneCount int64
	for i, f := range opts.Files {
		dst := filepath.Join(opts.DestDir, f.Name)
		if skipExisting {
			if info, err := os.Stat(dst); err == nil && !info.IsDir() && info.Size() == f.SizeBytes {
				results[i] = BulkDownloadResult{Name: f.Name, Path: dst, Skipped: true}
				n := atomic.AddInt64(&doneCount, 1)
				if opts.OnFileDone != nil {
					opts.OnFileDone(f.Name, f.SizeBytes, true, int(n), total)
				}
				continue
			}
		}
		toFetch = append(toFetch, f.Name)
	}

	// Presign every remaining file upfront in batches of 500. Simpler
	// than pipelining and finishes in seconds even for thousands of files.
	presigned := make([]PresignedDownload, 0, len(toFetch))
	for start := 0; start < len(toFetch); start += presignDownloadChunkSize {
		end := start + presignDownloadChunkSize
		if end > len(toFetch) {
			end = len(toFetch)
		}
		batch, err := c.PresignDownloadBulk(ctx, opts.CollectionID, toFetch[start:end])
		if err != nil {
			return results, fmt.Errorf("presign batch [%d..%d]: %w", start, end, err)
		}
		presigned = append(presigned, batch...)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, pd := range presigned {
		i, ok := byName[pd.Name]
		if !ok {
			continue // shouldn't happen — backend echoes our names
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, pd PresignedDownload) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = BulkDownloadResult{Name: pd.Name, Error: ctx.Err()}
				return
			}
			dst := filepath.Join(opts.DestDir, pd.Name)
			if err := getPresignedObject(ctx, pd.URL, dst); err != nil {
				results[i] = BulkDownloadResult{Name: pd.Name, Path: dst, Error: err}
				if opts.OnFileError != nil {
					opts.OnFileError(pd.Name, err)
				}
				return
			}
			results[i] = BulkDownloadResult{Name: pd.Name, Path: dst}
			n := atomic.AddInt64(&doneCount, 1)
			if opts.OnFileDone != nil {
				opts.OnFileDone(pd.Name, opts.Files[i].SizeBytes, false, int(n), total)
			}
		}(i, pd)
	}
	wg.Wait()
	return results, nil
}

// getPresignedObject GETs a presigned URL and writes the body to dstPath
// via a .part sidecar → rename dance so a killed process never leaves a
// truncated file at the final name (which SkipExisting would then treat
// as present-but-wrong-size on resume — mostly benign, but a rename is
// cheaper than debugging that ambiguity).
func getPresignedObject(ctx context.Context, presignedURL, dstPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, presignedURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("GET from S3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	tmp := dstPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, dstPath, err)
	}
	return nil
}
