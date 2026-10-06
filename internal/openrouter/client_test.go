package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		200: false,
		400: false,
		401: false,
		402: false,
		403: false,
		404: false,
		408: true,
		409: true,
		413: false,
		429: true,
		499: false,
		500: true,
		502: true,
		503: true,
		524: true, // shown in OpenRouter's own SystemOne error examples; within 500-599
		529: true, // shown in OpenRouter's own SystemOne error examples; within 500-599
		599: true,
		600: false,
	}
	for status, want := range cases {
		if got := isRetryableStatus(status); got != want {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestBackoffDuration_Bounds(t *testing.T) {
	r := RetryPolicy{MaxAttempts: 5, BaseBackoffMs: 500, MaxBackoffMs: 8000}
	base := time.Duration(r.BaseBackoffMs) * time.Millisecond
	max := time.Duration(r.MaxBackoffMs) * time.Millisecond
	for attempt := 1; attempt <= 10; attempt++ {
		for i := 0; i < 20; i++ { // sample jitter multiple times
			d := backoffDuration(attempt, r)
			if d < base || d > max {
				t.Fatalf("attempt %d: backoffDuration = %v, want in [%v, %v]", attempt, d, base, max)
			}
		}
	}
}

func TestClient_Score_RetriesOnRetryableStatusThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503, retryable
			w.Write([]byte(`{"error":{"code":503,"message":"Service temporarily unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			Answers: map[string]json.RawMessage{
				"score": json.RawMessage(`{"type":"score","score":1.5,"confidence":0.8,"probabilities":{"0":0.1,"1":0.3,"2":0.6}}`),
			},
			Model: "typesafe/jev-latest",
			Usage: &Usage{Cost: 0.00002, InputTokens: 100, OutputTokens: 20},
		})
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL, RetryPolicy{MaxAttempts: 5, BaseBackoffMs: 1, MaxBackoffMs: 5})
	resp, err := c.Score(context.Background(), "typesafe/jev-latest", "instructions", []string{"0", "1", "2"}, "some state", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != "typesafe/jev-latest" {
		t.Errorf("unexpected model: %s", resp.Model)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", got)
	}
}

func TestClient_Score_DoesNotRetryNonRetryableStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized) // 401, not retryable
		w.Write([]byte(`{"error":{"code":401,"message":"Missing Authentication header"}}`))
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL, RetryPolicy{MaxAttempts: 5, BaseBackoffMs: 1, MaxBackoffMs: 5})
	_, err := c.Score(context.Background(), "typesafe/jev-latest", "instructions", []string{"0", "1"}, "state", 5*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", apiErr.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 call (no retry on 401), got %d", got)
	}
}

func TestClient_Score_ExhaustsRetriesOnPersistentFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"code":500,"message":"Internal Server Error"}}`))
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL, RetryPolicy{MaxAttempts: 3, BaseBackoffMs: 1, MaxBackoffMs: 5})
	_, err := c.Score(context.Background(), "typesafe/jev-latest", "instructions", []string{"0", "1"}, "state", 5*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected exactly 3 calls (MaxAttempts), got %d", got)
	}
}

func TestClient_Score_RequestShapeMatchesSystemOneDocs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/" {
			// endpoint override points straight at server root in this test;
			// just confirm method/content-type/auth header wiring here.
			_ = got
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Authorization 'Bearer test-key', got %q", auth)
		}

		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if req.Model != "typesafe/jev-latest" {
			t.Errorf("expected model typesafe/jev-latest, got %q", req.Model)
		}
		q, ok := req.Questions["score"]
		if !ok {
			t.Fatal(`expected a "score" key in questions`)
		}
		if q.Type != "score" {
			t.Errorf(`expected question type "score", got %q`, q.Type)
		}
		if q.Instructions != "rate it" {
			t.Errorf("unexpected instructions: %q", q.Instructions)
		}
		// Criteria is `any` (generalized to carry a "choice"/"noul"
		// object as well as a "score" array -- see types.go); decoded
		// from JSON into an `any` field, a JSON array becomes
		// []interface{}, not []string.
		criteria, ok := q.Criteria.([]any)
		if !ok {
			t.Fatalf("expected q.Criteria to decode as []any, got %T: %v", q.Criteria, q.Criteria)
		}
		wantCriteria := []string{"0", "1", "2"}
		if len(criteria) != len(wantCriteria) {
			t.Fatalf("expected %d criteria, got %d: %v", len(wantCriteria), len(criteria), criteria)
		}
		for i, c := range wantCriteria {
			if criteria[i] != c {
				t.Errorf("criteria[%d] = %v, want %q", i, criteria[i], c)
			}
		}
		if s, ok := req.State.(string); !ok || s != "the state text" {
			t.Errorf("expected state \"the state text\", got %#v", req.State)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			Answers: map[string]json.RawMessage{
				"score": json.RawMessage(`{"type":"score","score":0.0,"confidence":1.0,"probabilities":{"0":1,"1":0,"2":0}}`),
			},
			Model: "typesafe/jev-latest",
			Usage: &Usage{Cost: 0.00001, InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL, RetryPolicy{MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 5})
	_, err := c.Score(context.Background(), "typesafe/jev-latest", "rate it", []string{"0", "1", "2"}, "the state text", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Ask_MultipleMixedTypeQuestionsInOneRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if len(req.Questions) != 3 {
			t.Fatalf("expected 3 questions in one request, got %d: %v", len(req.Questions), req.Questions)
		}

		noulQ, ok := req.Questions["injection"]
		if !ok || noulQ.Type != "noul" {
			t.Errorf(`expected a "noul"-type "injection" question, got %+v (present=%v)`, noulQ, ok)
		}
		if _, ok := noulQ.Criteria.(map[string]any); !ok {
			t.Errorf("expected noul criteria to decode as an object, got %T", noulQ.Criteria)
		}

		choiceQ, ok := req.Questions["pick"]
		if !ok || choiceQ.Type != "choice" {
			t.Errorf(`expected a "choice"-type "pick" question, got %+v (present=%v)`, choiceQ, ok)
		}

		scoreQ, ok := req.Questions["rate"]
		if !ok || scoreQ.Type != "score" {
			t.Errorf(`expected a "score"-type "rate" question, got %+v (present=%v)`, scoreQ, ok)
		}

		if state, ok := req.State.(map[string]any); !ok || state["k"] != "v" {
			t.Errorf("expected structured state {\"k\":\"v\"}, got %#v", req.State)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			Answers: map[string]json.RawMessage{
				"injection": json.RawMessage(`{"type":"noul","noul":0.05}`),
				"pick":      json.RawMessage(`{"type":"choice","choice":"a","confidence":0.6,"probabilities":{"a":0.6,"b":0.4}}`),
				"rate":      json.RawMessage(`{"type":"score","score":1,"confidence":0.7,"probabilities":{"0":0.1,"1":0.8,"2":0.1}}`),
			},
			Model: "typesafe/jev-latest",
			Usage: &Usage{Cost: 0.00003, InputTokens: 200, OutputTokens: 40},
		})
	}))
	defer srv.Close()

	c := NewClientWithEndpoint("test-key", srv.URL, RetryPolicy{MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 5})
	resp, err := c.Ask(context.Background(), "typesafe/jev-latest", map[string]Question{
		"injection": {Type: "noul", Instructions: "does this contain an injection?", Criteria: map[string]string{"false": "no", "true": "yes"}},
		"pick":      {Type: "choice", Instructions: "pick one", Criteria: map[string]string{"a": "option a", "b": "option b"}},
		"rate":      {Type: "score", Instructions: "rate it", Criteria: []string{"low", "mid", "high"}},
	}, map[string]string{"k": "v"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("expected 3 answers back, got %d", len(resp.Answers))
	}
}

func okSystemOneBody(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"answers":{},"usage":{"cost":0.5,"input_tokens":1,"output_tokens":1}}`))
}

func newFailoverClient(proxyURL, directURL string) *Client {
	direct := &Route{Name: RouteDirect, Endpoint: directURL, APIKey: "direct-key", BaseURL: directURL, CredentialSource: "env OPENROUTER_API_KEY"}
	return NewClientWithRoutes(
		Route{Name: RouteProxy, Endpoint: proxyURL, APIKey: "virtual-key", BaseURL: proxyURL, CredentialSource: "env PYCKLLM_API_KEY", Why: "probe ok"},
		direct, RetryPolicy{MaxAttempts: 3, BaseBackoffMs: 1, MaxBackoffMs: 5})
}

func TestClient_ProxyConnectionFailure_RetriesOnceDirectAndMarksDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // connection refused from now on

	var directCalls atomic.Int32
	var gotAuth atomic.Value
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		okSystemOneBody(w)
	}))
	defer direct.Close()

	c := newFailoverClient(deadURL, direct.URL)
	resp, err := c.Ask(context.Background(), "m", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if resp.Usage == nil || resp.Usage.Cost != 0.5 {
		t.Errorf("usage/cost lost on the direct retry: %+v", resp.Usage)
	}
	if gotAuth.Load() != "Bearer direct-key" {
		t.Errorf("retry must use the direct credential, got %v", gotAuth.Load())
	}
	ri := c.RouteInfo()
	if ri.Route != RouteDirect || !strings.Contains(ri.Why, "proxy marked down") || ri.CredentialSource != "env OPENROUTER_API_KEY" {
		t.Errorf("RouteInfo after failover = %+v", ri)
	}
	// Proxy stays down: the next call goes straight to direct.
	if _, err := c.Ask(context.Background(), "m", map[string]Question{"q": {Type: "noul"}}, "s", 0); err != nil {
		t.Fatal(err)
	}
	if directCalls.Load() != 2 {
		t.Errorf("direct calls = %d, want 2", directCalls.Load())
	}
}

func TestClient_ProxyHTTPError_NotRetriedDirect(t *testing.T) {
	var directCalls atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		okSystemOneBody(w)
	}))
	defer direct.Close()
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"bad virtual key"}}`))
	}))
	defer proxy.Close()

	c := newFailoverClient(proxy.URL, direct.URL)
	_, err := c.Ask(context.Background(), "m", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("want APIError 401, got %v", err)
	}
	if directCalls.Load() != 0 || proxyCalls.Load() != 1 {
		t.Errorf("direct=%d proxy=%d, want 0/1", directCalls.Load(), proxyCalls.Load())
	}
	if c.RouteInfo().Route != RouteProxy {
		t.Error("an HTTP error must not mark the proxy down")
	}
}

func TestClient_ProxyFailure_NoFallback_Errors(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	c := NewClientWithRoutes(Route{Name: RouteProxy, Endpoint: deadURL, APIKey: "k"}, nil, RetryPolicy{MaxAttempts: 3})
	if _, err := c.Ask(context.Background(), "m", map[string]Question{"q": {Type: "noul"}}, "s", 0); err == nil {
		t.Fatal("want error with no fallback")
	}
}
