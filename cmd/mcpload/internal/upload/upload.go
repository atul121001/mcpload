// Package upload POSTs a report.json to a server that accepts report uploads ({url}/api/v1/runs).
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Response is the server's reply to POST /api/v1/runs. Regression is kept
// raw because it may be a boolean or an object.
type Response struct {
	RunID      string          `json:"runId"`
	Status     string          `json:"status"`
	Regression json.RawMessage `json:"regression,omitempty"`
	URL        string          `json:"url"`
}

// Endpoint returns {base}/api/v1/runs.
func Endpoint(base string) string {
	return strings.TrimRight(base, "/") + "/api/v1/runs"
}

// Client posts reports.
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// Upload POSTs body (report JSON) to {base}/api/v1/runs with a bearer key.
// It returns the decoded response and the raw body.
func (c *Client) Upload(ctx context.Context, base, key string, body []byte) (*Response, []byte, error) {
	if base == "" {
		return nil, nil, fmt.Errorf("upload: upload URL is empty")
	}
	if key == "" {
		return nil, nil, fmt.Errorf("upload: API key is empty (pass --key or set MCPLOAD_KEY)")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint(base), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, raw, fmt.Errorf("upload: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var r Response
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, raw, fmt.Errorf("upload: decode response: %w", err)
		}
	}
	return &r, raw, nil
}
