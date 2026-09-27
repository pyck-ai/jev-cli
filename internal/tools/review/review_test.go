package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/tools/reviewcore"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *ReviewHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewReviewHandler(client, cfg, tracker, auditLog)
}

func goodAnswers() map[string]json.RawMessage {
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
			Usage:   &openrouter.Usage{Cost: 0.00005},
		})
	}))
}

func TestReviewHandler_Handle_OK_Auto(t *testing.T) {
	srv := fakeServer(t, goodAnswers())
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ReviewInput{
		Request: "add a widget", Diff: "+func Widget() {}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != reviewcore.ActionAuto {
		t.Errorf("Action = %q, want auto (reasons: %v)", out.Action, out.ReasonCodes)
	}
	if out.Model != "typesafe/jev-1.13-20260917" {
		t.Errorf("Model = %q", out.Model)
	}
	if out.Correctness.Score != 2 {
		t.Errorf("expected embedded Assessment fields to be promoted, Correctness = %+v", out.Correctness)
	}
}

func TestReviewHandler_Handle_MalformedRubricEscalates(t *testing.T) {
	answersMap := goodAnswers()
	delete(answersMap, reviewcore.QuestionCorrectness)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ReviewInput{
		Request: "add a widget", Diff: "+func Widget() {}",
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Action != reviewcore.ActionEscalate {
		t.Errorf("Action = %q, want escalate", out.Action)
	}
	if out.Correctness.Status != reviewcore.StatusInvalidResponse {
		t.Errorf("Correctness.Status = %q, want invalid_response", out.Correctness.Status)
	}
}

func TestReviewHandler_Handle_TruncationBlocksAuto(t *testing.T) {
	srv := fakeServer(t, goodAnswers())
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	longRequest := make([]byte, reviewcore.MaxRequestChars+1000)
	for i := range longRequest {
		longRequest[i] = 'x'
	}
	_, out, err := h.Handle(context.Background(), nil, ReviewInput{
		Request: string(longRequest), Diff: "+func Widget() {}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated {
		t.Error("expected Truncated = true")
	}
	if out.Action != reviewcore.ActionReview {
		t.Errorf("Action = %q, want review (truncated blocks auto)", out.Action)
	}
}

func TestReviewHandler_Handle_CustomAutoAcceptIsHonored(t *testing.T) {
	srv := fakeServer(t, goodAnswers()) // every rubric confidence is 0.95
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ReviewInput{
		Request: "add a widget", Diff: "+func Widget() {}",
		AutoAccept: 0.99, // stricter than the 0.95 confidences in goodAnswers()
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action == reviewcore.ActionAuto {
		t.Errorf("Action = auto, want non-auto given auto_accept=0.99 > every rubric's 0.95 confidence")
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(ReviewInput{Request: "r", Diff: "d"}); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]ReviewInput{
		"empty request":       {Request: " ", Diff: "d"},
		"empty diff":          {Request: "r", Diff: " "},
		"bad auto_accept":     {Request: "r", Diff: "d", AutoAccept: 0.5},
		"bad composite_floor": {Request: "r", Diff: "d", CompositeFloor: 1.5},
		"negative weight":     {Request: "r", Diff: "d", Weights: reviewcore.Weights{Correctness: -1}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

// TestValidateInput_ErrorsExplainWhatIsExpected: agents calling
// jev_review over MCP only see the error text, so each validation error
// must say what's wrong AND what's expected (field path, range, or
// default) -- not just what's wrong.
func TestValidateInput_ErrorsExplainWhatIsExpected(t *testing.T) {
	cases := map[string]struct {
		in   ReviewInput
		want []string
	}{
		"empty request": {
			ReviewInput{Request: " ", Diff: "d"},
			[]string{"request", "50,000 characters"},
		},
		"empty diff": {
			ReviewInput{Request: "r", Diff: " "},
			[]string{"diff", "50,000 characters"},
		},
		"bad auto_accept": {
			ReviewInput{Request: "r", Diff: "d", AutoAccept: 0.5},
			[]string{"auto_accept", "default 0.8"},
		},
		"bad composite_floor": {
			ReviewInput{Request: "r", Diff: "d", CompositeFloor: 1.5},
			[]string{"composite_floor", "[0,1]", "default 0.7"},
		},
		"negative weight": {
			ReviewInput{Request: "r", Diff: "d", Weights: reviewcore.Weights{Correctness: -1}},
			[]string{"weights.correctness", "normalized to sum to 1"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateInput(c.in)
			if err == nil {
				t.Fatalf("expected an error for case %q", name)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}
