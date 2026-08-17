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
}

// InitMultipartUpload starts a multipart upload for the given collection archive.
// sha256 is the lowercase-hex SHA-256 of the file, or empty to skip client-side
// dedup. Pass a hash for the per-file path where dedup is worth the cost; skip
// it for opaque archives where re-tar rarely produces byte-identical output.
func (c *Client) InitMultipartUpload(ctx context.Context, collectionID, filename, contentType, sha256 string) (*MultipartInitResp, error) {
	var resp MultipartInitResp
	body := multipartInitReq{CollectionID: collectionID, Filename: filename, ContentType: contentType, SHA256: sha256}
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
}
type presignBulkReq struct {
	CollectionID string            `json:"collectionId"`
	Files        []PresignBulkFile `json:"files"`
	// CreateArchived tells the backend to auto-materialize a *new*
	// collection in the archived state. Presigned URLs come back with
	// StorageClass=STANDARD_IA baked in — client MUST send matching
	// x-amz-storage-class header on each PUT.
	CreateArchived bool `json:"createArchived,omitempty"`
}
type PresignedFile struct {
	Name      string `json:"name"`
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int    `json:"expiresIn,omitempty"`
	// StorageClass, when set, is a signed header value that MUST be sent
	// as `x-amz-storage-class` on the PUT — otherwise S3 rejects with
	// 403 SignatureDoesNotMatch. Populated for createArchived=true.
	StorageClass string `json:"storageClass,omitempty"`
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
	return c.presignBulkInternal(ctx, collectionID, files, false)
}

// PresignBulkArchived is PresignBulk with createArchived=true. Backend
// materializes a brand-new archived collection; presigned URLs come
// back signed for StorageClass=STANDARD_IA so the client must send the
// x-amz-storage-class header on each PUT.
func (c *Client) PresignBulkArchived(ctx context.Context, collectionID string, files []PresignBulkFile) ([]PresignedFile, error) {
	return c.presignBulkInternal(ctx, collectionID, files, true)
}

func (c *Client) presignBulkInternal(ctx context.Context, collectionID string, files []PresignBulkFile, createArchived bool) ([]PresignedFile, error) {
	var resp presignBulkResp
	if err := c.postJSON(ctx, "/upload/presign", presignBulkReq{
		CollectionID:   collectionID,
		Files:          files,
		CreateArchived: createArchived,
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
	// CreateArchived=true materializes a brand-new archived collection
	// on this upload. Presigns come back with StorageClass=STANDARD_IA
	// baked in; the client MUST send x-amz-storage-class on each PUT.
	CreateArchived bool
	// Concurrency in-flight PUTs. Default 4 — S3 handles more but home
	// upstreams choke past that. CLI lets user override.
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
		names = append(names, PresignBulkFile{Name: name, ContentType: ct, SHA256: opts.SHA256s[i]})
	}
	for start := 0; start < len(names); start += presignBulkChunkSize {
		end := start + presignBulkChunkSize
		if end > len(names) {
			end = len(names)
		}
		batch, err := c.presignBulkInternal(ctx, opts.CollectionID, names[start:end], opts.CreateArchived)
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
			if err := putPresignedObject(ctx, pf.URL, f, info.Size(), contentTypeFor(pf.Name), pf.StorageClass); err != nil {
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
// signs with the same value we compute here. If storageClass is set
// (createArchived path), we also echo `x-amz-storage-class` — that
// header is part of the signed set when the SDK bakes StorageClass
// into the URL, and S3 rejects the PUT with 403 SignatureDoesNotMatch
// without the exact same value.
func putPresignedObject(ctx context.Context, presignedURL string, body io.Reader, length int64, contentType, storageClass string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, body)
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	if storageClass != "" {
		req.Header.Set("x-amz-storage-class", storageClass)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("PUT to S3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
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
}

// UploadFileMultipart orchestrates init → parallel part PUTs → complete for a
// seekable data source. Required for anything above the S3 5 GiB single-PUT
// ceiling; safe (if slightly chattier) for small files too.
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
	init, err := c.InitMultipartUpload(ctx, opts.CollectionID, opts.Filename, contentType, opts.SHA256)
	if err != nil {
		return nil, err
	}
	if init.Skipped {
		// Duplicate rejected before we started streaming — surface
		// as a MultipartCompleteResp with zero size and the duplicate
		// details in Key. Caller checks Skipped-shape by looking at
		// SizeBytes == 0 && Key == "" and the returned resp fields.
		return &MultipartCompleteResp{
			Skipped:     true,
			Reason:      init.Reason,
			DuplicateOf: init.DuplicateOf,
		}, nil
	}
	partSize := int64(init.PartSize)
	if partSize <= 0 {
		return nil, fmt.Errorf("backend returned zero partSize")
	}
	total := int((opts.Size + partSize - 1) / partSize)
	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}
	if concurrency > total {
		concurrency = total
	}

	completed := make([]CompletedPart, total)
	var uploadedBytes int64

	partCh := make(chan int, total)
	for i := 1; i <= total; i++ {
		partCh <- i
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
				url, err := c.SignMultipartPart(ctx, init.UploadID, init.Key, partNumber)
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

	return c.CompleteMultipartUpload(ctx, init.UploadID, init.Key, completed)
}

// putPresignedPart PUTs a single part and returns the ETag with quotes stripped.
// The URL already carries SigV4 auth in its query string, so we send no
// headers beyond Content-Length.
func putPresignedPart(ctx context.Context, presignedURL string, body io.Reader, length int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = length
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("PUT to S3: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("S3 returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", fmt.Errorf("S3 did not return an ETag")
	}
	return strings.Trim(etag, `"`), nil
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
