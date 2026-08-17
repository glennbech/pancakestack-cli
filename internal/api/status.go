package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// FetchOpsBanner hits the public GET /stats endpoint and returns just the
// `banner` field — the ops message ("planned downtime", "known-issue: uploads
// are slow", etc.). Unauthenticated on purpose; the marketing widget uses the
// same endpoint. Best-effort: any transport, HTTP, or decode error yields
// ("", nil) so the caller can no-op silently. A hard 1.5s timeout keeps CLI
// startup fast when the network's flaky.
func FetchOpsBanner(ctx context.Context, baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + "/stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil
	}
	var body struct {
		Banner string `json:"banner"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", nil
	}
	return strings.TrimSpace(body.Banner), nil
}
