package ask

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

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *AskHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewAskHandler(client, cfg, tracker, auditLog)
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

func TestAskHandler_Handle_OK_AllThreeTypes(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"q_noul":   json.RawMessage(`{"type":"noul","noul":0.8}`),
		"q_choice": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.7,"b":0.3}}`),
		"q_score":  json.RawMessage(`{"type":"score","score":1.5,"confidence":0.6,"probabilities":{"0":0.1,"1":0.3,"2":0.6}}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, AskInput{
		State: "some state",
		Questions: map[string]AskQuestion{
			"q_noul":   {Type: "noul", Instructions: "is it?", Criteria: map[string]any{"true": "yes", "false": "no"}},
			"q_choice": {Type: "choice", Instructions: "pick", Criteria: map[string]any{"a": "option a", "b": "option b"}},
			"q_score":  {Type: "score", Instructions: "rate", Criteria: []any{"low", "mid", "high"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Answers) != 3 {
		t.Fatalf("len(Answers) = %d, want 3", len(out.Answers))
	}
	if a := out.Answers["q_noul"]; a.Status != StatusOK || a.Noul == nil || *a.Noul != 0.8 {
		t.Errorf("q_noul = %+v", a)
	}
	if a := out.Answers["q_choice"]; a.Status != StatusOK || a.Choice != "a" || a.Confidence == nil || *a.Confidence != 0.7 {
		t.Errorf("q_choice = %+v", a)
	}
	if a := out.Answers["q_score"]; a.Status != StatusOK || a.Score == nil || *a.Score != 1.5 {
		t.Errorf("q_score = %+v", a)
	}
}

func TestAskHandler_Handle_OneMalformedAnswerPreservesOthers(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"good": json.RawMessage(`{"type":"noul","noul":0.5}`),
		// "bad" missing entirely
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, AskInput{
		State: "s",
		Questions: map[string]AskQuestion{
			"good": {Type: "noul", Instructions: "x", Criteria: map[string]any{"true": "t", "false": "f"}},
			"bad":  {Type: "noul", Instructions: "y", Criteria: map[string]any{"true": "t", "false": "f"}},
		},
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Answers["good"].Status != StatusOK {
		t.Errorf("good = %+v, want ok", out.Answers["good"])
	}
	bad := out.Answers["bad"]
	if bad.Status != StatusInvalidResponse {
		t.Errorf("bad.Status = %q, want invalid_response", bad.Status)
	}
	if bad.Type != "noul" {
		t.Errorf("bad.Type = %q, want noul (type is preserved even when invalid)", bad.Type)
	}
	if bad.Noul != nil {
		t.Errorf("bad.Noul = %v, want nil (never fabricated)", *bad.Noul)
	}
}

func TestValidateInput(t *testing.T) {
	valid := AskInput{
		State: "s",
		Questions: map[string]AskQuestion{
			"q": {Type: "noul", Instructions: "i", Criteria: map[string]any{"true": "t", "false": "f"}},
		},
	}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}

	cases := map[string]AskInput{
		"nil state": {
			State: nil,
			Questions: map[string]AskQuestion{
				"q": {Type: "noul", Instructions: "i", Criteria: map[string]any{"true": "t"}},
			},
		},
		"no questions": {State: "s", Questions: nil},
		"unknown type": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "banana", Instructions: "i", Criteria: map[string]any{"true": "t"}},
			},
		},
		"noul criteria as array (wrong shape)": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "noul", Instructions: "i", Criteria: []any{"true", "false"}},
			},
		},
		"score criteria as object (wrong shape)": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "score", Instructions: "i", Criteria: map[string]any{"0": "low"}},
			},
		},
		"score criteria with non-string element": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "score", Instructions: "i", Criteria: []any{"low", 42}},
			},
		},
		"empty instructions": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "noul", Instructions: "  ", Criteria: map[string]any{"true": "t"}},
			},
		},
		"empty criteria object": {
			State: "s",
			Questions: map[string]AskQuestion{
				"q": {Type: "noul", Instructions: "i", Criteria: map[string]any{}},
			},
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_TooManyQuestions(t *testing.T) {
	qs := make(map[string]AskQuestion, maxQuestions+1)
	for i := 0; i < maxQuestions+1; i++ {
		qs[string(rune('a'))+string(rune(i))] = AskQuestion{Type: "noul", Instructions: "i", Criteria: map[string]any{"true": "t"}}
	}
	if err := validateInput(AskInput{State: "s", Questions: qs}); err == nil {
		t.Error("expected error for too many questions")
	}
}
