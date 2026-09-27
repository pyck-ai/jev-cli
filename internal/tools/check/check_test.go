package check

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) (*CheckHandler, *budget.Tracker, string) {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	auditLog := audit.NewLogger(auditPath)
	return NewCheckHandler(client, cfg, tracker, auditLog), tracker, auditPath
}

func fakeServer(t *testing.T, answers map[string]json.RawMessage, usage *openrouter.Usage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   usage,
		})
	}))
}

func TestCheckHandler_Handle_OK(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"p0": json.RawMessage(`{"type":"noul","noul":0.92}`), // likely
		"p1": json.RawMessage(`{"type":"noul","noul":0.05}`), // unlikely
		"p2": json.RawMessage(`{"type":"noul","noul":0.5}`),  // uncertain
	}, &openrouter.Usage{Cost: 0.00002, InputTokens: 100, OutputTokens: 20})
	defer srv.Close()

	h, tracker, _ := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, CheckInput{
		Propositions: []string{"the sky is blue", "the sky is green", "it might rain"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3", len(out.Results))
	}
	wantLabels := []string{"likely", "unlikely", "uncertain"}
	wantActions := []string{ActionAuto, ActionAuto, ActionReview}
	for i, r := range out.Results {
		if r.Status != StatusOK {
			t.Errorf("Results[%d].Status = %q, want ok", i, r.Status)
		}
		if r.Label != wantLabels[i] {
			t.Errorf("Results[%d].Label = %q, want %q", i, r.Label, wantLabels[i])
		}
		if r.Action != wantActions[i] {
			t.Errorf("Results[%d].Action = %q, want %q", i, r.Action, wantActions[i])
		}
	}
	if tracker.Total() != 0.00002 {
		t.Errorf("tracked spend = %v, want 0.00002", tracker.Total())
	}
}

func TestCheckHandler_Handle_MalformedAnswerFailsClosedPerProposition(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"p0": json.RawMessage(`{"type":"noul","noul":0.9}`),
		// p1 missing entirely
	}, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	h, _, auditPath := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, CheckInput{
		Propositions: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Results[0].Status != StatusOK {
		t.Errorf("Results[0].Status = %q, want ok", out.Results[0].Status)
	}
	r1 := out.Results[1]
	if r1.Status != StatusInvalidResponse {
		t.Errorf("Results[1].Status = %q, want invalid_response", r1.Status)
	}
	if r1.Action != ActionReview {
		t.Errorf("Results[1].Action = %q, want review (never auto on invalid_response)", r1.Action)
	}
	if r1.Probability != 0 || r1.Label != "" {
		t.Errorf("expected zero-valued placeholders on invalid_response, got probability=%v label=%q", r1.Probability, r1.Label)
	}

	data, rerr := os.ReadFile(auditPath)
	if rerr != nil {
		t.Fatalf("reading audit log: %v", rerr)
	}
	var entry audit.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("audit log line is not valid JSON: %v", err)
	}
	if entry.ItemCount != 2 || entry.InvalidCount != 1 {
		t.Errorf("audit ItemCount/InvalidCount = %d/%d, want 2/1", entry.ItemCount, entry.InvalidCount)
	}
	if entry.Status != StatusInvalidResponse {
		t.Errorf("audit status = %q, want %q (>=1 invalid item taints the whole entry)", entry.Status, StatusInvalidResponse)
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(CheckInput{Propositions: []string{"x"}}); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	if err := validateInput(CheckInput{Propositions: nil}); err == nil {
		t.Error("expected error for empty propositions")
	}
	tooMany := make([]string, maxPropositions+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	if err := validateInput(CheckInput{Propositions: tooMany}); err == nil {
		t.Error("expected error for too many propositions")
	}
	if err := validateInput(CheckInput{Propositions: []string{"  "}}); err == nil {
		t.Error("expected error for a blank proposition")
	}
	if err := validateInput(CheckInput{Propositions: []string{"x"}, AutoAccept: 0.3}); err == nil {
		t.Error("expected error for auto_accept <= 0.5")
	}
}

func TestCheckHandler_Handle_SessionBudgetRefusesBeforeCalling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("expected the OpenRouter endpoint to never be called once session budget is exhausted")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Budget.MaxUSDPerSession = 1.0
	h, tracker, _ := newTestHandler(t, srv.URL, cfg)
	tracker.Add(1.0)

	_, _, err := h.Handle(context.Background(), nil, CheckInput{Propositions: []string{"x"}})
	if err == nil {
		t.Fatal("expected refusal error when session budget already exhausted")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("expected budget-related error, got %v", err)
	}
}

// TestValidateInput_ErrorsExplainTheExpectedShape: agents calling
// jev_check over MCP only see the error text, so each validation error
// must say what the right shape/value is, not just what was wrong.
func TestValidateInput_ErrorsExplainTheExpectedShape(t *testing.T) {
	cases := map[string]struct {
		in   CheckInput
		want string
	}{
		"empty propositions": {
			CheckInput{Propositions: nil},
			fmt.Sprintf("provide 1 to %d propositions", maxPropositions),
		},
		"too many propositions": {
			CheckInput{Propositions: tooManyPropositions()},
			fmt.Sprintf("more than the maximum of %d", maxPropositions),
		},
		"blank proposition": {CheckInput{Propositions: []string{"  "}}, "propositions[0]: must not be empty"},
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

func tooManyPropositions() []string {
	props := make([]string, maxPropositions+1)
	for i := range props {
		props[i] = "x"
	}
	return props
}
