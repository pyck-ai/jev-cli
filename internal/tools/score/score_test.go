package score

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// --- parseScoreAnswer -------------------------------------------------

func TestParseScoreAnswer_Valid_ZeroBased(t *testing.T) {
	answers := map[string]json.RawMessage{
		"score": json.RawMessage(`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99},"legend":{"0":"a","1":"b","2":"c"}}`),
	}
	score, confidence, probs, valid := parseScoreAnswer(answers, 0, 3)
	if !valid {
		t.Fatalf("expected valid=true")
	}
	if score != 1.99 {
		t.Errorf("score = %v, want 1.99", score)
	}
	if confidence != 0.99 {
		t.Errorf("confidence = %v, want 0.99", confidence)
	}
	want := map[string]float64{"0": 0, "1": 0.01, "2": 0.99}
	if len(probs) != len(want) {
		t.Fatalf("probabilities = %v, want %v", probs, want)
	}
	for k, v := range want {
		if probs[k] != v {
			t.Errorf("probabilities[%q] = %v, want %v", k, probs[k], v)
		}
	}
}

func TestParseScoreAnswer_Valid_RemapsNonZeroScaleMin(t *testing.T) {
	// OpenRouter always returns 0-based indices (see openrouter package doc
	// comment); scaleMin=5 with 3 levels should remap indices 0,1,2 to the
	// real levels 5,6,7, and offset the score accordingly.
	answers := map[string]json.RawMessage{
		"score": json.RawMessage(`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`),
	}
	score, _, probs, valid := parseScoreAnswer(answers, 5, 3)
	if !valid {
		t.Fatalf("expected valid=true")
	}
	if score != 5+1.99 {
		t.Errorf("score = %v, want %v", score, 5+1.99)
	}
	wantKeys := []string{"5", "6", "7"}
	for _, k := range wantKeys {
		if _, ok := probs[k]; !ok {
			t.Errorf("expected remapped probabilities to contain key %q, got %v", k, probs)
		}
	}
	if _, ok := probs["2"]; ok {
		t.Errorf("did not expect raw OpenRouter index key %q to leak into remapped probabilities", "2")
	}
	if probs["7"] != 0.99 {
		t.Errorf(`probs["7"] = %v, want 0.99 (remapped from OpenRouter index "2")`, probs["7"])
	}
}

func TestParseScoreAnswer_Invalid(t *testing.T) {
	cases := map[string]map[string]json.RawMessage{
		"missing score key": {},
		"wrong answer type": {
			"score": json.RawMessage(`{"type":"choice","choice":"x"}`),
		},
		"malformed json": {
			"score": json.RawMessage(`not json at all`),
		},
		"missing a level": {
			// levels=3 (indices 0,1,2 expected) but index "2" absent
			"score": json.RawMessage(`{"type":"score","score":0.5,"confidence":0.5,"probabilities":{"0":0.5,"1":0.5}}`),
		},
		"probabilities do not sum to 1": {
			"score": json.RawMessage(`{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":0.1,"1":0.1,"2":0.1}}`),
		},
		"probability out of range": {
			"score": json.RawMessage(`{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":-0.5,"1":0.5,"2":1.0}}`),
		},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, valid := parseScoreAnswer(answers, 0, 3)
			if valid {
				t.Errorf("expected valid=false for case %q", name)
			}
		})
	}
}

func TestParseScoreAnswer_ToleratesSumWithin001(t *testing.T) {
	// sum = 0.995, within the documented 0.01 tolerance of 1.0
	answers := map[string]json.RawMessage{
		"score": json.RawMessage(`{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":0.005,"1":0.49,"2":0.5}}`),
	}
	_, _, _, valid := parseScoreAnswer(answers, 0, 3)
	if !valid {
		t.Errorf("expected sum-within-tolerance probabilities to be valid")
	}
}

// --- validateInput ------------------------------------------------------

func TestValidateInput(t *testing.T) {
	valid := ScoreInput{State: "x", Instructions: "y", ScaleMin: 0, ScaleMax: 2}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}

	cases := map[string]ScoreInput{
		"empty state":            {State: "  ", Instructions: "y", ScaleMin: 0, ScaleMax: 2},
		"empty instructions":     {State: "x", Instructions: "", ScaleMin: 0, ScaleMax: 2},
		"scale_max == scale_min": {State: "x", Instructions: "y", ScaleMin: 2, ScaleMax: 2},
		"scale_max < scale_min":  {State: "x", Instructions: "y", ScaleMin: 5, ScaleMax: 2},
		"scale range too large":  {State: "x", Instructions: "y", ScaleMin: 0, ScaleMax: 100000},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

// --- ScoreHandler.Handle end-to-end (against a fake OpenRouter server) --

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) (*ScoreHandler, *budget.Tracker, string) {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	auditLog := audit.NewLogger(auditPath)
	return NewScoreHandler(client, cfg, tracker, auditLog), tracker, auditPath
}

func fakeSystemOneServer(t *testing.T, answerJSON string, usage *openrouter.Usage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"score": json.RawMessage(answerJSON)},
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   usage,
		})
	}))
}

func TestScoreHandler_Handle_OK(t *testing.T) {
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`,
		&openrouter.Usage{Cost: 0.000019992, InputTokens: 476, OutputTokens: 70})
	defer srv.Close()

	cfg := config.Default()
	h, tracker, auditPath := newTestHandler(t, srv.URL, cfg)

	_, out, err := h.Handle(context.Background(), nil, ScoreInput{
		State:        "diff content to judge",
		ScaleMin:     0,
		ScaleMax:     2,
		Instructions: "0=incorrect, 1=partially correct, 2=fully correct",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusOK {
		t.Errorf("status = %q, want %q", out.Status, StatusOK)
	}
	if out.Score != 1.99 {
		t.Errorf("score = %v, want 1.99", out.Score)
	}
	if out.Confidence != 0.99 {
		t.Errorf("confidence = %v, want 0.99", out.Confidence)
	}
	if out.Model != "typesafe/jev-1.13-20260917" {
		t.Errorf("model = %q", out.Model)
	}
	if out.Usage == nil || out.Usage.InputTokens != 476 || out.Usage.OutputTokens != 70 {
		t.Errorf("usage = %+v, want input=476 output=70", out.Usage)
	}
	if out.LatencyMs < 0 {
		t.Errorf("latency_ms = %d, want >= 0", out.LatencyMs)
	}
	if out.BudgetExceeded {
		t.Errorf("did not expect budget_exceeded for a $0.00002 call under the $0.01 default cap")
	}
	if got := tracker.Total(); got != 0.000019992 {
		t.Errorf("session spend tracked = %v, want 0.000019992", got)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	var entry audit.Entry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("audit log line is not valid JSON: %v\nline: %s", err, line)
	}
	if entry.Status != StatusOK {
		t.Errorf("audit status = %q, want %q", entry.Status, StatusOK)
	}
	if entry.InputStateSHA256 != audit.HashState("diff content to judge") {
		t.Errorf("audit input_state_sha256 mismatch")
	}
	if strings.Contains(line, "diff content to judge") {
		t.Errorf("audit log must not contain the raw judged state text, got: %s", line)
	}
}

func TestScoreHandler_Handle_InvalidResponse(t *testing.T) {
	// probabilities don't sum to 1 -> must fail closed, not a Go error.
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":1,"confidence":0.5,"probabilities":{"0":0.1,"1":0.1,"2":0.1}}`,
		&openrouter.Usage{Cost: 0.00001, InputTokens: 50, OutputTokens: 10})
	defer srv.Close()

	cfg := config.Default()
	h, _, _ := newTestHandler(t, srv.URL, cfg)

	_, out, err := h.Handle(context.Background(), nil, ScoreInput{
		State: "x", ScaleMin: 0, ScaleMax: 2, Instructions: "y",
	})
	if err != nil {
		t.Fatalf("expected a structured invalid_response result, not a Go error: %v", err)
	}
	if out.Status != StatusInvalidResponse {
		t.Errorf("status = %q, want %q", out.Status, StatusInvalidResponse)
	}
	if out.Score != 0 || out.Confidence != 0 || out.Probabilities != nil {
		t.Errorf("expected zero-valued placeholders on invalid_response, got score=%v confidence=%v probabilities=%v",
			out.Score, out.Confidence, out.Probabilities)
	}
}

func TestScoreHandler_Handle_TransportErrorIsGoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"Missing Authentication header"}}`))
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Retry.MaxAttempts = 1
	h, _, auditPath := newTestHandler(t, srv.URL, cfg)

	_, _, err := h.Handle(context.Background(), nil, ScoreInput{
		State: "x", ScaleMin: 0, ScaleMax: 2, Instructions: "y",
	})
	if err == nil {
		t.Fatal("expected a Go error for a hard API failure")
	}

	data, rerr := os.ReadFile(auditPath)
	if rerr != nil {
		t.Fatalf("reading audit log: %v", rerr)
	}
	var entry audit.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("audit log line is not valid JSON: %v", err)
	}
	if entry.Status != "error" {
		t.Errorf(`audit status = %q, want "error"`, entry.Status)
	}
	if entry.Error == "" {
		t.Errorf("expected a non-empty error message in the audit entry")
	}
}

func TestScoreHandler_Handle_PerCallBudgetExceededIsFlaggedNotRejected(t *testing.T) {
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":1,"confidence":0.9,"probabilities":{"0":0,"1":1,"2":0}}`,
		&openrouter.Usage{Cost: 5.00, InputTokens: 100, OutputTokens: 20}) // way over the cap
	defer srv.Close()

	cfg := config.Default()
	cfg.Budget.MaxUSDPerCall = 0.01 // default; the $5 response cost must exceed it
	h, tracker, _ := newTestHandler(t, srv.URL, cfg)

	_, out, err := h.Handle(context.Background(), nil, ScoreInput{
		State: "x", ScaleMin: 0, ScaleMax: 2, Instructions: "y",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusOK {
		t.Errorf(`status = %q, want "ok" (a valid answer that merely cost too much is flagged, not invalidated)`, out.Status)
	}
	if !out.BudgetExceeded {
		t.Errorf("expected budget_exceeded=true for a $5.00 call against a $0.01 cap")
	}
	if got := tracker.Total(); got != 5.00 {
		t.Errorf("session spend = %v, want 5.00 (the call was already paid for and must still be tracked)", got)
	}
}

func TestScoreHandler_Handle_SessionBudgetRefusesBeforeCalling(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Budget.MaxUSDPerSession = 1.0
	h, tracker, auditPath := newTestHandler(t, srv.URL, cfg)
	tracker.Add(1.0) // already at the cap from "previous calls" this session

	_, _, err := h.Handle(context.Background(), nil, ScoreInput{
		State: "x", ScaleMin: 0, ScaleMax: 2, Instructions: "y",
	})
	if err == nil {
		t.Fatal("expected refusal error when session budget already exhausted")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("expected the OpenRouter endpoint to never be called once session budget is exhausted, got %d calls", got)
	}

	data, rerr := os.ReadFile(auditPath)
	if rerr != nil {
		t.Fatalf("reading audit log: %v", rerr)
	}
	var entry audit.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("audit log line is not valid JSON: %v", err)
	}
	if entry.Status != "error" {
		t.Errorf(`audit status = %q, want "error"`, entry.Status)
	}
}

// TestValidateInput_ErrorsExplainTheExpectedShape: agents calling
// jev_score over MCP only see the error text, so each validation error
// must say what the right shape/value is, not just what was wrong.
func TestValidateInput_ErrorsExplainTheExpectedShape(t *testing.T) {
	cases := map[string]struct {
		in   ScoreInput
		want string
	}{
		"empty state": {
			ScoreInput{State: "  ", Instructions: "y", ScaleMin: 0, ScaleMax: 2},
			"state: must not be empty",
		},
		"empty instructions": {
			ScoreInput{State: "x", Instructions: "", ScaleMin: 0, ScaleMax: 2},
			"describe what each scale level means",
		},
		"scale_max <= scale_min": {
			ScoreInput{State: "x", Instructions: "y", ScaleMin: 5, ScaleMax: 2},
			"scale_max: must be greater than scale_min",
		},
		"scale range too large": {
			ScoreInput{State: "x", Instructions: "y", ScaleMin: 0, ScaleMax: 100000},
			"range too large",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateInput(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
