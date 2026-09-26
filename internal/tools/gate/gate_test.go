package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/tools/reviewcore"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *GateHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewGateHandler(client, cfg, tracker, auditLog)
}

func goodReviewAnswers() map[string]json.RawMessage {
	best := `{"type":"score","score":2,"confidence":0.95,"probabilities":{"0":0.02,"1":0.03,"2":0.95}}`
	worst := `{"type":"score","score":0,"confidence":0.95,"probabilities":{"0":0.95,"1":0.03,"2":0.02}}`
	return map[string]json.RawMessage{
		reviewcore.QuestionCorrectness: json.RawMessage(best),
		reviewcore.QuestionSpecMatch:   json.RawMessage(best),
		reviewcore.QuestionTestGap:     json.RawMessage(worst),
		reviewcore.QuestionBlastRadius: json.RawMessage(worst),
		reviewcore.QuestionSafeToApply: json.RawMessage(`{"type":"noul","noul":0.97}`),
	}
}

func fakeServer(t *testing.T, answers map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00006},
		})
	}))
}

func baseInput() GateInput {
	return GateInput{
		Request: "add a widget", Diff: "+func Widget() {}",
		Claims:   []string{"the widget is thread-safe"},
		Evidence: []EvidenceItem{{ID: "e1", Text: "Widget has no shared mutable state."}},
	}
}

func TestGateHandler_Handle_AllAutoIsAuto(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != ActionAuto {
		t.Errorf("Action = %q, want auto (reasons: %v)", out.Action, out.ReasonCodes)
	}
	if out.Verification.Summary.Auto != 1 || out.Verification.Summary.Contradicted != 0 {
		t.Errorf("Summary = %+v", out.Verification.Summary)
	}
}

func TestGateHandler_Handle_ConfidentlyContradictedForcesEscalate(t *testing.T) {
	// Review half is perfect/auto, but the claim is confidently contradicted.
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.95,"probabilities":{"supports":0.02,"contradicts":0.95,"says_nothing":0.03}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != ActionEscalate {
		t.Errorf("Action = %q, want escalate (a confidently contradicted claim must force escalate even though review is healthy)", out.Action)
	}
	if out.Verification.Summary.Contradicted != 1 {
		t.Errorf("Summary.Contradicted = %d, want 1", out.Verification.Summary.Contradicted)
	}
}

func TestGateHandler_Handle_ReviewEscalateForcesGateEscalate(t *testing.T) {
	answersMap := goodReviewAnswers()
	delete(answersMap, reviewcore.QuestionCorrectness) // makes the review half escalate
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Review.Action != reviewcore.ActionEscalate {
		t.Fatalf("test setup: Review.Action = %q, want escalate", out.Review.Action)
	}
	if out.Action != ActionEscalate {
		t.Errorf("Action = %q, want escalate (review half escalated)", out.Action)
	}
}

func TestGateHandler_Handle_NonAutoClaimIsJustReview(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"says_nothing","confidence":0.5,"probabilities":{"supports":0.3,"contradicts":0.2,"says_nothing":0.5}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != ActionReview {
		t.Errorf("Action = %q, want review (low-confidence non-contradicting claim, healthy review)", out.Action)
	}
}

func TestGateHandler_Handle_MalformedClaimFailsClosed(t *testing.T) {
	answersMap := goodReviewAnswers() // claim0 missing entirely
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	r := out.Verification.Results[0]
	if r.Status != StatusInvalidResponse || r.Action != ActionReview {
		t.Errorf("claim result = %+v, want invalid_response/review", r)
	}
	if out.Action != ActionReview {
		t.Errorf("Action = %q, want review (not escalate: an invalid claim isn't a *confident* contradiction)", out.Action)
	}
	if out.Verification.Summary.Invalid != 1 {
		t.Errorf("Summary.Invalid = %d, want 1", out.Verification.Summary.Invalid)
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(baseInput()); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]GateInput{
		"no claims":             {Request: "r", Diff: "d", Evidence: []EvidenceItem{{ID: "e", Text: "t"}}},
		"no evidence":           {Request: "r", Diff: "d", Claims: []string{"c"}},
		"duplicate evidence id": {Request: "r", Diff: "d", Claims: []string{"c"}, Evidence: []EvidenceItem{{ID: "e", Text: "t"}, {ID: "e", Text: "t2"}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_AggregateEvidenceCap(t *testing.T) {
	big := strings.Repeat("x", maxEvidenceAggChars/2+1)
	in := GateInput{
		Request: "r", Diff: "d", Claims: []string{"c"},
		Evidence: []EvidenceItem{{ID: "a", Text: big}, {ID: "b", Text: big}},
	}
	if err := validateInput(in); err == nil {
		t.Error("expected error for aggregate evidence text over cap")
	}
}

func TestValidateInput_TooManyClaims(t *testing.T) {
	claims := make([]string, maxClaims+1)
	for i := range claims {
		claims[i] = "c"
	}
	in := baseInput()
	in.Claims = claims
	if err := validateInput(in); err == nil {
		t.Error("expected error for too many claims")
	}
}
