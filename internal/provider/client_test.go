package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stub is a local stand-in for the LumaTrack ingest endpoint. Every test in
// this package talks to one of these; none of them talk to production.
type stub struct {
	server   *httptest.Server
	requests []recorded
	status   int
	body     string
	// retryAfter, when set, is sent as the Retry-After header on the FIRST
	// response only, so a retry can be observed landing on a 201.
	retryAfter string
	calls      int32
}

type recorded struct {
	method string
	path   string
	auth   string
	ctype  string
	body   map[string]any
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{status: http.StatusCreated, body: `{"run":{"id":"run_01ABC","held":false,"status":"success"},"deduplicated":false}`}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		parsed := map[string]any{}
		_ = json.Unmarshal(raw, &parsed)
		s.requests = append(s.requests, recorded{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			ctype:  r.Header.Get("Content-Type"),
			body:   parsed,
		})
		n := atomic.AddInt32(&s.calls, 1)
		if s.retryAfter != "" && n == 1 {
			w.Header().Set("Retry-After", s.retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate limit exceeded."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) client() *Client {
	c := NewClient(s.server.URL, "lmt_test_key")
	c.RetryWait = time.Millisecond
	return c
}

func TestRecordRunPostsToTheRunsEndpoint(t *testing.T) {
	s := newStub(t)

	got, err := s.client().RecordRun(context.Background(), RunRequest{
		Automation: "os-patching",
		Status:     "success",
		Source:     "terraform",
	})
	if err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}

	if len(s.requests) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(s.requests))
	}
	req := s.requests[0]
	if req.method != http.MethodPost {
		t.Errorf("method: want POST, got %s", req.method)
	}
	if req.path != "/api/v1/runs" {
		t.Errorf("path: want /api/v1/runs, got %s", req.path)
	}
	if req.auth != "Bearer lmt_test_key" {
		t.Errorf("auth header: want %q, got %q", "Bearer lmt_test_key", req.auth)
	}
	if req.ctype != "application/json" {
		t.Errorf("content-type: want application/json, got %q", req.ctype)
	}
	if req.body["automation"] != "os-patching" {
		t.Errorf("automation: got %v", req.body["automation"])
	}
	if got.Run.ID != "run_01ABC" {
		t.Errorf("run id: want run_01ABC, got %q", got.Run.ID)
	}
	if got.Deduplicated {
		t.Error("deduplicated: want false")
	}
}

// A trailing slash on the endpoint is the most common operator typo; it must
// not produce a double-slashed path the router 404s on.
func TestRecordRunTrimsTrailingSlashFromEndpoint(t *testing.T) {
	s := newStub(t)
	c := NewClient(s.server.URL+"/", "lmt_test_key")

	if _, err := c.RecordRun(context.Background(), RunRequest{Automation: "os-patching"}); err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}
	if s.requests[0].path != "/api/v1/runs" {
		t.Errorf("path: want /api/v1/runs, got %s", s.requests[0].path)
	}
}

// Zero-valued optionals must be omitted rather than sent as 0 or "": a
// duration_seconds of 0 is a real measurement and units of 0 is a real count,
// so the client can never invent either one.
func TestRecordRunOmitsUnsetOptionalFields(t *testing.T) {
	s := newStub(t)

	_, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}

	body := s.requests[0].body
	for _, key := range []string{"duration_seconds", "units", "external_id", "failure_reason", "executed_at", "metadata", "status"} {
		if _, present := body[key]; present {
			t.Errorf("%q must be omitted when unset, got %v", key, body[key])
		}
	}
}

func TestRecordRunSendsZeroDurationAndUnitsWhenSet(t *testing.T) {
	s := newStub(t)
	zero := int64(0)

	_, err := s.client().RecordRun(context.Background(), RunRequest{
		Automation:      "os-patching",
		DurationSeconds: &zero,
		Units:           &zero,
	})
	if err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}

	body := s.requests[0].body
	if body["duration_seconds"] != float64(0) {
		t.Errorf("duration_seconds: want 0, got %v", body["duration_seconds"])
	}
	if body["units"] != float64(0) {
		t.Errorf("units: want 0, got %v", body["units"])
	}
}

func TestRecordRunSendsEveryOptionalFieldWhenSet(t *testing.T) {
	s := newStub(t)
	duration := int64(142)
	units := int64(240)

	_, err := s.client().RecordRun(context.Background(), RunRequest{
		Automation:      "os-patching",
		Status:          "failure",
		DurationSeconds: &duration,
		Units:           &units,
		ExternalID:      "tf-apply-99412",
		Source:          "terraform",
		FailureReason:   "auth/credential",
		ExecutedAt:      "2026-06-11T14:30:00Z",
		Metadata:        map[string]string{"workspace": "prod"},
	})
	if err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}

	body := s.requests[0].body
	want := map[string]any{
		"automation":       "os-patching",
		"status":           "failure",
		"duration_seconds": float64(142),
		"units":            float64(240),
		"external_id":      "tf-apply-99412",
		"source":           "terraform",
		"failure_reason":   "auth/credential",
		"executed_at":      "2026-06-11T14:30:00Z",
	}
	for key, expected := range want {
		if body[key] != expected {
			t.Errorf("%s: want %v, got %v", key, expected, body[key])
		}
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata: want an object, got %T", body["metadata"])
	}
	if metadata["workspace"] != "prod" {
		t.Errorf("metadata.workspace: want prod, got %v", metadata["workspace"])
	}
}

// A 200 is the API's idempotent replay: the external_id was already recorded.
// That is a success, and the caller has to be able to tell it apart from a 201.
func TestRecordRunReportsDeduplicatedReplay(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusOK
	s.body = `{"run":{"id":"run_01ABC","held":false},"deduplicated":true}`

	got, err := s.client().RecordRun(context.Background(), RunRequest{
		Automation: "os-patching",
		ExternalID: "tf-apply-99412",
	})
	if err != nil {
		t.Fatalf("a replay is not an error: %v", err)
	}
	if !got.Deduplicated {
		t.Error("deduplicated: want true on a 200 replay")
	}
}

// 202 means the run was recorded but held over the plan's monthly cap. Held
// runs earn nothing until the plan is raised, so the operator has to hear it.
func TestRecordRunReportsHeldRun(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusAccepted
	s.body = `{"run":{"id":"run_01ABC","held":true},"deduplicated":false}`

	got, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err != nil {
		t.Fatalf("a held run is recorded, not an error: %v", err)
	}
	if !got.Run.Held {
		t.Error("held: want true on a 202")
	}
}

// The API answers errors as {"error": "..."} written for a human. Surfacing
// that message is the whole point; a bare "HTTP 404" would send the operator
// digging for a typo the server already named.
func TestRecordRunSurfacesTheServerErrorMessage(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusNotFound
	s.body = `{"error":"No automation with slug 'nope'. See GET /api/v1/automations."}`

	_, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "nope"})
	if err == nil {
		t.Fatal("want an error on a 404")
	}
	if !strings.Contains(err.Error(), "No automation with slug 'nope'") {
		t.Errorf("error must carry the server message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error must carry the status code, got: %v", err)
	}
}

func TestRecordRunErrorsOnAnUnparseableBody(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusBadGateway
	s.body = `<html>gateway timeout</html>`

	_, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err == nil {
		t.Fatal("want an error when the body is not the documented JSON")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error must carry the status code, got: %v", err)
	}
}

// Rate limits are counted per organization across every key, so a wide apply
// can genuinely hit one. A 429 carrying Retry-After is a "wait this long",
// and honoring it once is the difference between a recorded run and a lost one.
func TestRecordRunRetriesOnceAfterRetryAfter(t *testing.T) {
	s := newStub(t)
	s.retryAfter = "1"

	got, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err != nil {
		t.Fatalf("want the retry to succeed, got: %v", err)
	}
	if len(s.requests) != 2 {
		t.Fatalf("want 2 requests (original + retry), got %d", len(s.requests))
	}
	if got.Run.ID != "run_01ABC" {
		t.Errorf("run id from the retry: got %q", got.Run.ID)
	}
}

// A monthly event-cap 429 never carries Retry-After. Retrying it would burn
// the window against a ceiling that will not move for days, so it must fail
// straight through with the server's explanation.
func TestRecordRunDoesNotRetryA429WithoutRetryAfter(t *testing.T) {
	s := newStub(t)
	s.status = http.StatusTooManyRequests
	s.body = `{"error":"This month's event ceiling is reached."}`

	_, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err == nil {
		t.Fatal("want an error on a cap 429")
	}
	if len(s.requests) != 1 {
		t.Errorf("want exactly 1 request, got %d", len(s.requests))
	}
	if !strings.Contains(err.Error(), "event ceiling") {
		t.Errorf("error must carry the server message, got: %v", err)
	}
}

// ignored_fields means the server did not understand part of the payload. It
// records the run anyway, so a silent success here is how a misspelled field
// quietly stops booking cost.
func TestRecordRunReturnsIgnoredFields(t *testing.T) {
	s := newStub(t)
	s.body = `{"run":{"id":"run_01ABC","held":false},"deduplicated":false,"ignored_fields":["ai_usage"]}`

	got, err := s.client().RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err != nil {
		t.Fatalf("RecordRun returned an error: %v", err)
	}
	if len(got.IgnoredFields) != 1 || got.IgnoredFields[0] != "ai_usage" {
		t.Errorf("ignored_fields: want [ai_usage], got %v", got.IgnoredFields)
	}
}

func TestRecordRunFailsWithoutAnAPIKey(t *testing.T) {
	s := newStub(t)
	c := NewClient(s.server.URL, "")

	_, err := c.RecordRun(context.Background(), RunRequest{Automation: "os-patching"})
	if err == nil {
		t.Fatal("want an error when no API key is configured")
	}
	if len(s.requests) != 0 {
		t.Errorf("must not send an unauthenticated request, got %d", len(s.requests))
	}
}
