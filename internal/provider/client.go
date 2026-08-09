package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultEndpoint is the hosted LumaTrack API. Self-hosted installs override
// it in the provider block or with LUMATRACK_ENDPOINT.
const DefaultEndpoint = "https://lumatrack.io"

// maxErrorBody caps how much of a non-JSON error body we quote back. A
// misrouted request can return a whole HTML page; the first line is the useful
// part and the rest is noise in a Terraform diagnostic.
const maxErrorBody = 300

// Client speaks the runs-ingest contract: POST /api/v1/runs with a bearer key.
type Client struct {
	Endpoint string
	APIKey   string
	HTTP     *http.Client

	// RetryWait caps how long a Retry-After pause may last. A rate-limit 429
	// names its own wait, but an apply must not hang for an arbitrary
	// server-chosen interval, so the shorter of the two wins.
	RetryWait time.Duration
}

func NewClient(endpoint, apiKey string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		Endpoint:  trimTrailingSlashes(endpoint),
		APIKey:    apiKey,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		RetryWait: 30 * time.Second,
	}
}

func trimTrailingSlashes(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// RunRequest is the subset of the ingest contract this provider reports.
// Pointers mark the fields where zero is a real measurement (a zero-second run,
// a zero-unit run) and so must be distinguishable from "not set".
type RunRequest struct {
	Automation      string            `json:"automation"`
	Status          string            `json:"status,omitempty"`
	DurationSeconds *int64            `json:"duration_seconds,omitempty"`
	Units           *int64            `json:"units,omitempty"`
	ExternalID      string            `json:"external_id,omitempty"`
	Source          string            `json:"source,omitempty"`
	FailureReason   string            `json:"failure_reason,omitempty"`
	ExecutedAt      string            `json:"executed_at,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// RunResponse is the ingest reply. An ingest-only key gets identity and hold
// state only, so every evidence field here may be absent by design.
type RunResponse struct {
	Run struct {
		ID     string `json:"id"`
		Held   bool   `json:"held"`
		Status string `json:"status"`
	} `json:"run"`
	Deduplicated  bool     `json:"deduplicated"`
	Warning       string   `json:"warning"`
	IgnoredFields []string `json:"ignored_fields"`
}

// RecordRun reports one run event. A 200 (idempotent replay), 201 (recorded)
// and 202 (recorded but held over the plan cap) are all successes; the caller
// tells them apart from Deduplicated and Run.Held.
func (c *Client) RecordRun(ctx context.Context, run RunRequest) (*RunResponse, error) {
	if c.APIKey == "" {
		return nil, errors.New("no LumaTrack API key is configured: set it in the provider block or LUMATRACK_API_KEY")
	}
	if run.Automation == "" {
		return nil, errors.New("automation (the slug) is required")
	}

	body, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("could not encode the run: %w", err)
	}

	response, retryAfter, err := c.post(ctx, body)
	// A 429 that names its own wait is a rate limit, and the window is a fixed
	// minute. One retry converts it into a recorded run. A cap 429 carries no
	// Retry-After and is not retried: the ceiling will not move today.
	if retryAfter > 0 {
		wait := min(retryAfter, c.RetryWait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		response, _, err = c.post(ctx, body)
	}
	return response, err
}

// post sends one request. It returns a positive retryAfter only when the reply
// is a retryable rate-limit 429, which is what makes the cap 429 fall through
// to the error path untouched.
func (c *Client) post(ctx context.Context, body []byte) (*RunResponse, time.Duration, error) {
	url := c.Endpoint + "/api/v1/runs"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("could not build the request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("could not reach LumaTrack at %s: %w", c.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("could not read the LumaTrack response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		if wait, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			return nil, wait, nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, 0, fmt.Errorf("LumaTrack answered HTTP %d: %s", resp.StatusCode, errorMessage(raw))
	}

	parsed := &RunResponse{}
	if err := json.Unmarshal(raw, parsed); err != nil {
		return nil, 0, fmt.Errorf("LumaTrack answered HTTP %d with a body this provider could not parse: %s", resp.StatusCode, truncate(raw))
	}
	return parsed, 0, nil
}

// parseRetryAfter reads the delay-seconds form the API documents. A zero or
// negative value is treated as absent so it cannot spin.
func parseRetryAfter(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// errorMessage pulls the human-written message out of {"error": "..."} and
// falls back to the raw body when the reply is not the documented shape (a
// proxy's HTML 502, say).
func errorMessage(raw []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return truncate(raw)
}

func truncate(raw []byte) string {
	text := string(bytes.TrimSpace(raw))
	if text == "" {
		return "(empty body)"
	}
	if len(text) > maxErrorBody {
		return text[:maxErrorBody] + "..."
	}
	return text
}
