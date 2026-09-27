package compare

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
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *CompareHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewCompareHandler(client, cfg, tracker, auditLog)
}

func fakeServer(t *testing.T, answers map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00002},
		})
	}))
}

func TestCompareHandler_Handle_OverallOnly(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.9,"probabilities":{"same_fact":0.05,"contradicts":0.9,"different_facts":0.05}}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, CompareInput{PassageA: "the sky is blue", PassageB: "the sky is red"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Overall.Relation != RelationContradicts || out.Overall.Decision != DecisionAuto {
		t.Errorf("Overall = %+v", out.Overall)
	}
	if len(out.Aspects) != 0 {
		t.Errorf("expected no aspects, got %v", out.Aspects)
	}
	if out.Truncated {
		t.Error("did not expect Truncated for short passages")
	}
}

func TestCompareHandler_Handle_WithAspects(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"different_facts","confidence":0.7,"probabilities":{"same_fact":0.1,"contradicts":0.2,"different_facts":0.7}}`),
		"aspect0": json.RawMessage(`{"type":"choice","choice":"same_fact","confidence":0.85,"probabilities":{"same_fact":0.85,"contradicts":0.05,"different_facts":0.1}}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, CompareInput{
		PassageA: "a", PassageB: "b", Aspects: []string{"pricing"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Aspects) != 1 || out.Aspects[0].Aspect != "pricing" || out.Aspects[0].Relation != RelationSameFact {
		t.Fatalf("Aspects = %+v", out.Aspects)
	}
	if out.Aspects[0].Decision != DecisionAuto {
		t.Errorf("Aspects[0].Decision = %q, want auto", out.Aspects[0].Decision)
	}
}

func TestCompareHandler_Handle_MalformedAnswerFailsClosed(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		// overall missing entirely
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, CompareInput{PassageA: "a", PassageB: "b"})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Overall.Status != StatusInvalidResponse || out.Overall.Decision != DecisionReview {
		t.Errorf("Overall = %+v, want invalid_response/review", out.Overall)
	}
	if out.Overall.Relation != "" || out.Overall.Confidence != 0 {
		t.Errorf("expected zero-valued placeholders on invalid_response, got %+v", out.Overall)
	}
}

func TestCompareHandler_Handle_TruncatesOverlongPassages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		state, _ := req.State.(map[string]any)
		a, _ := state["passage_a"].(string)
		if len([]rune(a)) != maxPassageChars {
			t.Errorf("passage_a length sent to model = %d, want exactly %d", len([]rune(a)), maxPassageChars)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"overall": json.RawMessage(`{"type":"choice","choice":"same_fact","confidence":0.5,"probabilities":{"same_fact":0.5,"contradicts":0.25,"different_facts":0.25}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	longA := strings.Repeat("x", maxPassageChars+1000)
	_, out, err := h.Handle(context.Background(), nil, CompareInput{PassageA: longA, PassageB: "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated {
		t.Error("expected Truncated = true")
	}
}

func TestValidateInput(t *testing.T) {
	if err := validateInput(CompareInput{PassageA: "a", PassageB: "b"}); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]CompareInput{
		"empty passage_a": {PassageA: " ", PassageB: "b"},
		"empty passage_b": {PassageA: "a", PassageB: " "},
		"blank aspect":    {PassageA: "a", PassageB: "b", Aspects: []string{" "}},
		"bad auto_accept": {PassageA: "a", PassageB: "b", AutoAccept: 2.0},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

// TestValidateInput_ErrorsExplainTheExpectedShape guards against the
// regression that motivated describing every field and enriching every
// validation error (see compare.go's jsonschema tags and validateInput).
// See internal/tools/ask's identically-named test for the precedent
// this follows.
func TestValidateInput_ErrorsExplainTheExpectedShape(t *testing.T) {
	cases := map[string]struct {
		in   CompareInput
		want string
	}{
		"empty passage_a": {CompareInput{PassageA: " ", PassageB: "b"}, "provide the first passage"},
		"empty passage_b": {CompareInput{PassageA: "a", PassageB: " "}, "provide the second passage"},
		"blank aspect":    {CompareInput{PassageA: "a", PassageB: "b", Aspects: []string{" "}}, "pricing"},
		"too many aspects": {
			CompareInput{PassageA: "a", PassageB: "b", Aspects: make([]string, maxAspects+1)},
			"compare fewer aspects per call",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// validateInput checks len(Aspects) before any per-aspect
			// content, so a slice of blank strings still exercises the
			// "too many aspects" branch, not the "blank aspect" one.
			err := validateInput(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
