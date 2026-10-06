package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Route names. A Route is one way of reaching SystemOne: directly at
// OpenRouter, or through the local LiteLLM proxy that forwards
// /openrouter/api/v1/* to OpenRouter unchanged (see internal/route).
const (
	RouteDirect = "direct"
	RouteProxy  = "proxy"
)

// Route is one target a Client can send to: the full SystemOne endpoint
// URL and the bearer credential for it, plus descriptive fields for
// jev_doctor. APIKey is deliberately excluded from JSON and from
// RouteInfo; nothing in this package prints it.
type Route struct {
	Name             string // RouteDirect or RouteProxy
	Endpoint         string // full URL of the systemone endpoint
	APIKey           string `json:"-"`
	BaseURL          string // API base (".../api/v1"), for reporting only
	CredentialSource string // where APIKey came from; never the key
	Why              string // why this route was chosen (probe result)
}

// RouteInfo is the key-free view of the currently active route, for
// jev_doctor and logs.
type RouteInfo struct {
	Route            string
	Why              string
	BaseURL          string
	CredentialSource string
}

// RetryPolicy configures the client's retry/backoff behavior. It mirrors
// config.Retry but is defined independently here so this package has no
// dependency on internal/config and can be used/tested standalone.
type RetryPolicy struct {
	MaxAttempts   int
	BaseBackoffMs int
	MaxBackoffMs  int
}

// Client is a minimal HTTP client for OpenRouter's SystemOne API.
//
// It sends to one active Route. When that is the proxy and a direct
// fallback Route exists, a connection-level failure (see doWithRetry)
// switches the Client to the fallback for the rest of the process and
// retries the failing call once there; the request body and headers are
// identical on both routes.
type Client struct {
	httpClient *http.Client
	retry      RetryPolicy

	mu       sync.Mutex
	active   Route
	fallback *Route // nil: no failover possible
}

// NewClient creates a Client for the real OpenRouter SystemOne endpoint.
// apiKey is sent as a Bearer token on every request and is never logged or
// otherwise surfaced by this package.
func NewClient(apiKey string, retry RetryPolicy) *Client {
	return NewClientWithEndpoint(apiKey, Endpoint, retry)
}

// NewClientWithEndpoint is like NewClient but targets an arbitrary
// endpoint URL instead of the real OpenRouter SystemOne endpoint. It
// exists so tests (in this package and in internal/tools) can point the
// client at an httptest.Server; production code should use NewClient.
func NewClientWithEndpoint(apiKey, endpoint string, retry RetryPolicy) *Client {
	return NewClientWithRoutes(Route{Name: RouteDirect, Endpoint: endpoint, APIKey: apiKey, BaseURL: BaseURL, CredentialSource: "explicit", Why: "constructed directly"}, nil, retry)
}

// NewClientWithRoutes creates a Client that sends to primary and, if
// primary is the proxy and fallback is non-nil, fails over to fallback on
// a connection-level proxy failure.
func NewClientWithRoutes(primary Route, fallback *Route, retry RetryPolicy) *Client {
	return &Client{
		httpClient: &http.Client{},
		retry:      retry,
		active:     primary,
		fallback:   fallback,
	}
}

// RouteInfo reports the currently active route (after any failover).
func (c *Client) RouteInfo() RouteInfo {
	r := c.current()
	return RouteInfo{Route: r.Name, Why: r.Why, BaseURL: r.BaseURL, CredentialSource: r.CredentialSource}
}

func (c *Client) current() Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// failOver switches from the failed route to the fallback, if the failed
// route is the proxy and a fallback exists. It returns the route to use
// next and whether it is a switch (true) or no failover is possible
// (false). Concurrent calls that fail at the same time all end up on the
// fallback; only the first records the reason.
func (c *Client) failOver(failed Route, cause error) (Route, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if failed.Name != RouteProxy || c.fallback == nil {
		return failed, false
	}
	if c.active.Name == RouteProxy {
		next := *c.fallback
		next.Why = "proxy marked down after connection failure: " + cause.Error()
		c.active = next
	}
	return c.active, true
}

// APIError represents a non-2xx HTTP response from OpenRouter (after
// retries, if any, have been exhausted, or immediately for a non-retryable
// status). StatusCode is the HTTP status; Code is OpenRouter's
// error.code from the JSON body when present (normally equal to
// StatusCode).
type APIError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: HTTP %d: %s", e.StatusCode, e.Message)
}

// Score submits a single "score"-type SystemOne question for the given
// model/state/instructions/criteria and returns the parsed response
// envelope. It is a thin wrapper around Ask, preserving jev_score's
// original, pre-generalization method signature and wire format exactly
// (see this package's doc comment, "Generalization beyond \"score\"").
//
// Semantic validation of the "score" answer (presence, probability
// coverage/sum, scale_min/scale_max remapping) is intentionally NOT done
// here: it is the caller's responsibility (see
// internal/tools/score/score.go and internal/answers), since correct
// validation depends on scale_min/scale_max, which this package has no
// opinion about.
func (c *Client) Score(ctx context.Context, model string, instructions string, criteria []string, state string, timeout time.Duration) (*Response, error) {
	return c.Ask(ctx, model, map[string]Question{
		"score": {
			Type:         "score",
			Instructions: instructions,
			Criteria:     criteria,
		},
	}, state, timeout)
}

// Ask submits an arbitrary set of named SystemOne questions -- of mixed
// types, e.g. several "choice" questions and a "noul" question in the
// same call (see this package's doc comment) -- in a single HTTP request,
// and returns the response envelope. Per-question answer parsing and
// validation is the caller's responsibility (see internal/answers and
// each internal/tools/* package), since each question type has a
// different answer shape and different validation rules, and a single
// call's questions map may mix several types at once.
//
// timeout, if > 0, bounds the TOTAL time spent across all retry attempts
// (it is applied once, wrapping the whole retry loop, not per-attempt) per
// the project brief's request_timeout_ms semantics.
func (c *Client) Ask(ctx context.Context, model string, questions map[string]Question, state any, timeout time.Duration) (*Response, error) {
	reqBody := Request{
		Model:     model,
		Questions: questions,
		State:     state,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openrouter: marshaling request: %w", err)
	}

	status, respBody, err := c.doWithRetry(ctx, http.MethodPost, func(r Route) string { return r.Endpoint }, body, timeout)
	out, err := parseAskResponse(status, respBody)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseAskResponse turns one completed HTTP exchange into a Response or
// an error (*APIError for a non-2xx status or an error envelope).
func parseAskResponse(status int, respBody []byte) (*Response, error) {
	if status < 200 || status >= 300 {
		return nil, parseAPIError(status, respBody)
	}

	// Defensive check: OpenRouter's docs show that, for the streaming
	// /chat/completions endpoint, a completed HTTP 200 response can still
	// carry an {"error": {...}} body instead of real content once headers
	// have already been sent (see "Non-streaming requests" under
	// "Mid-stream errors" in OpenRouter's Errors and Debugging docs).
	// SystemOne is not documented as a streaming endpoint and its own docs
	// show errors with distinct non-2xx status codes, so this is not
	// expected to trigger in practice -- but since it was not possible to
	// verify with a live call, we check for it defensively rather than
	// assume it can't happen.
	var maybeErr errorEnvelope
	if json.Unmarshal(respBody, &maybeErr) == nil && maybeErr.Error.Message != "" {
		return nil, &APIError{StatusCode: status, Code: maybeErr.Error.Code, Message: maybeErr.Error.Message}
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("openrouter: parsing response body: %w", err)
	}
	return &out, nil
}

// doWithRetry performs the HTTP round trip, retrying on retryable status
// codes with jittered exponential backoff, up to retry.MaxAttempts total
// attempts, all within a single `timeout` deadline covering every attempt.
//
// Per the project brief: retry ONLY on HTTP 408/409/429/500-599 status
// codes actually received from the server. Any error returned by
// http.Client.Do (connection refused, connection reset, TLS handshake
// failure, DNS failure, context deadline exceeded while the round trip was
// in flight, etc.) is treated as an "ambiguous network failure" and is
// NEVER retried, because we cannot tell whether the server ever received
// and processed the request -- retrying could double-charge. The same
// applies to a failure while reading the response body after a request was
// successfully sent: we already don't know if the (unread) body would have
// indicated success, so retrying is not safe.
//
// target picks the request URL from the route in use, so a failover to
// the direct route also switches the URL. A nil body sends no body (and
// no Content-Type).
func (c *Client) doWithRetry(ctx context.Context, method string, target func(Route) string, body []byte, timeout time.Duration) (int, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxAttempts := c.retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	route := c.current()
	failedOver := false
	for attempt := 1; ; attempt++ {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target(route), bodyReader)
		if err != nil {
			return 0, nil, fmt.Errorf("openrouter: building request: %w", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+route.APIKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// A connection-level failure to the PROXY (refused, reset,
			// DNS, dial timeout: Do returned an error and no response
			// arrived) while the caller's deadline is still live: the
			// proxy is unusable, so retry this call once on the direct
			// route and keep using it. HTTP error responses never reach
			// this branch -- they are real answers and are not retried
			// on another route.
			if !failedOver && ctx.Err() == nil {
				if next, ok := c.failOver(route, err); ok {
					route, failedOver = next, true
					attempt--
					continue
				}
			}
			return 0, nil, fmt.Errorf("openrouter: request failed (not retried, ambiguous network error): %w", err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, fmt.Errorf("openrouter: reading response body (not retried, ambiguous network error): %w", readErr)
		}

		if !isRetryableStatus(resp.StatusCode) || attempt >= maxAttempts {
			return resp.StatusCode, respBody, nil
		}

		if !c.sleepBackoff(ctx, attempt) {
			return 0, nil, fmt.Errorf("openrouter: request_timeout_ms deadline reached during retry backoff after HTTP %d: %w", resp.StatusCode, ctx.Err())
		}
	}
}

// isRetryableStatus reports whether an HTTP status code returned by
// OpenRouter should be retried, per the project brief: 408, 409, 429, or
// any 5xx (500-599). This also covers the 524 (timeout) and 529 (provider
// overloaded) codes shown in OpenRouter's own SystemOne error examples,
// since both fall within 500-599.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

// sleepBackoff waits out the backoff delay for the given (1-based) attempt
// number, or returns false early if ctx is done first.
func (c *Client) sleepBackoff(ctx context.Context, attempt int) bool {
	d := backoffDuration(attempt, c.retry)
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoffDuration computes a jittered exponential backoff delay to wait
// after the given (1-based) attempt before the next one.
//
// The delay is jittered uniformly within [BaseBackoffMs, ceiling], where
// ceiling grows exponentially with the attempt number up to MaxBackoffMs.
// This is an "equal jitter" style scheme, chosen (over e.g. AWS's "full
// jitter", which allows delays anywhere down to 0) specifically so that
// every delay honors the brief's literal wording -- "jittered exponential
// backoff between base_backoff_ms and max_backoff_ms" -- as a hard lower
// bound of base_backoff_ms and hard upper bound of max_backoff_ms, not just
// an upper bound.
func backoffDuration(attempt int, r RetryPolicy) time.Duration {
	base := time.Duration(r.BaseBackoffMs) * time.Millisecond
	maxD := time.Duration(r.MaxBackoffMs) * time.Millisecond
	if maxD < base {
		maxD = base
	}

	ceiling := base
	for i := 1; i < attempt; i++ {
		if ceiling >= maxD {
			ceiling = maxD
			break
		}
		ceiling *= 2
	}
	if ceiling > maxD {
		ceiling = maxD
	}
	if ceiling <= base {
		return base
	}

	span := int64(ceiling - base)
	return base + time.Duration(rand.Int64N(span+1))
}

// parseAPIError builds an APIError from a non-2xx response body, using
// OpenRouter's documented {"error": {"code", "message"}} envelope when the
// body matches it, and falling back to the raw body text otherwise.
func parseAPIError(status int, respBody []byte) error {
	var env errorEnvelope
	if err := json.Unmarshal(respBody, &env); err == nil && env.Error.Message != "" {
		return &APIError{StatusCode: status, Code: env.Error.Code, Message: env.Error.Message}
	}
	return &APIError{StatusCode: status, Code: status, Message: string(respBody)}
}
