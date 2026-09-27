package decide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *DecideHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewDecideHandler(client, cfg, tracker, auditLog)
}

func baseInput() DecideInput {
	return DecideInput{
		Decision:   "which vendor to pick",
		Evidence:   "vendor A is cheaper, vendor B is faster",
		Priorities: "cost matters most",
		Candidates: []Candidate{
			{ID: "a", Description: "Vendor A"},
			{ID: "b", Description: "Vendor B"},
		},
	}
}

func TestDecideHandler_Handle_OK_NoRequirements(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Questions) != 1 {
			t.Fatalf("expected exactly 1 question (no requirements), got %d", len(req.Questions))
		}
		q := req.Questions["decision"]
		criteria, _ := q.Criteria.(map[string]any)
		for _, hatch := range []string{EscapeAskUser, EscapeInvestigate, EscapeNone} {
			if _, ok := criteria[hatch]; !ok {
				t.Errorf("expected escape hatch %q in criteria (default true), got %v", hatch, criteria)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.7,"b":0.1,"ask_user":0.1,"investigate":0.05,"none":0.05}}`),
			},
			Model: "typesafe/jev-1.13-20260917",
			Usage: &openrouter.Usage{Cost: 0.00003},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Recommendation.Selected != "a" || out.Recommendation.Escaped {
		t.Errorf("Recommendation = %+v, want selected=a escaped=false", out.Recommendation)
	}
	if len(out.Checks) != 0 {
		t.Errorf("expected no checks, got %v", out.Checks)
	}
}

func TestDecideHandler_Handle_EscapeHatchSelected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"decision": json.RawMessage(`{"type":"choice","choice":"ask_user","confidence":0.8,"probabilities":{"a":0.2,"b":0.2,"ask_user":0.5,"investigate":0.05,"none":0.05}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, baseInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Recommendation.Selected != EscapeAskUser || !out.Recommendation.Escaped {
		t.Errorf("Recommendation = %+v, want selected=ask_user escaped=true", out.Recommendation)
	}
}

func TestDecideHandler_Handle_EscapeHatchesDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		criteria, _ := req.Questions["decision"].Criteria.(map[string]any)
		if _, ok := criteria[EscapeAskUser]; ok {
			t.Errorf("did not expect escape hatches in criteria when disabled, got %v", criteria)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	in := baseInput()
	f := false
	in.EscapeHatches = &f
	_, out, err := h.Handle(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Recommendation.Escaped {
		t.Error("Escaped should be false when escape hatches are disabled and a real candidate was chosen")
	}
}

func TestDecideHandler_Handle_WithRequirements(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		// 1 main + 1 requirement * 2 candidates = 3
		if len(req.Questions) != 3 {
			t.Fatalf("expected 3 questions, got %d: %v", len(req.Questions), req.Questions)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.6,"b":0.1,"ask_user":0.1,"investigate":0.1,"none":0.1}}`),
				"req0_a":   json.RawMessage(`{"type":"choice","choice":"supported","confidence":0.9,"probabilities":{"supported":0.9,"contradicted":0.05,"unclear":0.05}}`),
				"req0_b":   json.RawMessage(`{"type":"choice","choice":"contradicted","confidence":0.8,"probabilities":{"supported":0.1,"contradicted":0.8,"unclear":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00004},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	in := baseInput()
	in.Requirements = []string{"must support SSO"}
	_, out, err := h.Handle(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Checks) != 2 {
		t.Fatalf("len(Checks) = %d, want 2", len(out.Checks))
	}
	byCandidate := map[string]Check{}
	for _, c := range out.Checks {
		byCandidate[c.Candidate] = c
	}
	if byCandidate["a"].Answer != AnswerSupported || byCandidate["a"].Requirement != 0 {
		t.Errorf("checks[a] = %+v", byCandidate["a"])
	}
	if byCandidate["b"].Answer != AnswerContradicted {
		t.Errorf("checks[b] = %+v", byCandidate["b"])
	}
}

func TestDecideHandler_Handle_MalformedAnswersFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				// "decision" missing; "req0_a" missing; "req0_b" present
				"req0_b": json.RawMessage(`{"type":"choice","choice":"unclear","confidence":0.5,"probabilities":{"supported":0.3,"contradicted":0.2,"unclear":0.5}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	in := baseInput()
	in.Requirements = []string{"must support SSO"}
	_, out, err := h.Handle(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Recommendation.Status != StatusInvalidResponse {
		t.Errorf("Recommendation.Status = %q, want invalid_response", out.Recommendation.Status)
	}
	if out.Recommendation.Selected != "" || out.Recommendation.Escaped {
		t.Errorf("expected zero-valued placeholders, got %+v", out.Recommendation)
	}
	byCandidate := map[string]Check{}
	for _, c := range out.Checks {
		byCandidate[c.Candidate] = c
	}
	if byCandidate["a"].Answer != AnswerInvalidResponse {
		t.Errorf("checks[a].Answer = %q, want invalid_response", byCandidate["a"].Answer)
	}
	if byCandidate["b"].Answer != AnswerUnclear {
		t.Errorf("checks[b].Answer = %q, want unclear", byCandidate["b"].Answer)
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(baseInput()); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]DecideInput{
		"empty decision":         {Evidence: "e", Priorities: "p", Candidates: []Candidate{{ID: "a", Description: "d"}, {ID: "b", Description: "d"}}},
		"too few candidates":     {Decision: "d", Evidence: "e", Priorities: "p", Candidates: []Candidate{{ID: "a", Description: "d"}}},
		"reserved candidate id":  {Decision: "d", Evidence: "e", Priorities: "p", Candidates: []Candidate{{ID: "none", Description: "d"}, {ID: "b", Description: "d"}}},
		"duplicate candidate id": {Decision: "d", Evidence: "e", Priorities: "p", Candidates: []Candidate{{ID: "a", Description: "d"}, {ID: "a", Description: "d"}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_TooManyCandidates(t *testing.T) {
	in := baseInput()
	in.Candidates = append(in.Candidates,
		Candidate{ID: "c", Description: "d"}, Candidate{ID: "d", Description: "d"},
		Candidate{ID: "e", Description: "d"}, Candidate{ID: "f", Description: "d"},
		Candidate{ID: "g", Description: "d"},
	)
	if err := validateInput(in); err == nil {
		t.Error("expected error for more than 6 candidates")
	}
}

func TestValidateInput_TooManyRequirements(t *testing.T) {
	in := baseInput()
	reqs := make([]string, maxRequirements+1)
	for i := range reqs {
		reqs[i] = "req"
	}
	in.Requirements = reqs
	if err := validateInput(in); err == nil {
		t.Error("expected error for too many requirements")
	}
}
