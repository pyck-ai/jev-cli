package verify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *VerifyHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewVerifyHandler(client, cfg, tracker, auditLog)
}

func fakeServer(t *testing.T, answers map[string]json.RawMessage, checkReq func(*testing.T, openrouter.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkReq != nil {
			var req openrouter.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decoding request: %v", err)
			}
			checkReq(t, req)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00002},
		})
	}))
}

func TestVerifyHandler_Handle_OK_StringEvidence(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`),
		"c1": json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.85,"probabilities":{"supports":0.05,"contradicts":0.85,"says_nothing":0.1}}`),
	}, func(t *testing.T, req openrouter.Request) {
		if s, ok := req.State.(string); !ok || s != "the sky is blue" {
			t.Errorf("expected state to be the evidence string, got %#v", req.State)
		}
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, VerifyInput{
		Claims:   []string{"the sky is blue", "the sky is green"},
		Evidence: "the sky is blue",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(out.Results))
	}
	if out.Results[0].Verdict != VerdictSupports || out.Results[0].Action != ActionAuto {
		t.Errorf("Results[0] = %+v", out.Results[0])
	}
	if out.Results[1].Verdict != VerdictContradicts || out.Results[1].Action != ActionAuto {
		t.Errorf("Results[1] = %+v", out.Results[1])
	}
}

func TestVerifyHandler_Handle_OK_StructuredEvidence(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"says_nothing","confidence":0.7,"probabilities":{"supports":0.1,"contradicts":0.2,"says_nothing":0.7}}`),
	}, func(t *testing.T, req openrouter.Request) {
		items, ok := req.State.([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("expected state to be a 1-element array, got %#v", req.State)
		}
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, VerifyInput{
		Claims: []string{"unrelated claim"},
		Evidence: []any{
			map[string]any{"id": "doc1", "text": "some evidence text"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Results[0].Verdict != VerdictSaysNothing {
		t.Errorf("Verdict = %q, want %q", out.Results[0].Verdict, VerdictSaysNothing)
	}
}

func TestVerifyHandler_Handle_LowConfidenceIsReview(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.55,"probabilities":{"supports":0.55,"contradicts":0.25,"says_nothing":0.2}}`),
	}, nil)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, VerifyInput{
		Claims:   []string{"a claim"},
		Evidence: "some evidence",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Results[0].Action != ActionReview {
		t.Errorf("Action = %q, want %q for confidence 0.55 < default auto_accept 0.8", out.Results[0].Action, ActionReview)
	}
}

func TestVerifyHandler_Handle_MalformedAnswerFailsClosed(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"maybe","confidence":0.9}`), // "maybe" isn't a valid option
	}, nil)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, VerifyInput{
		Claims:   []string{"a claim"},
		Evidence: "some evidence",
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	r := out.Results[0]
	if r.Status != StatusInvalidResponse {
		t.Errorf("Status = %q, want %q", r.Status, StatusInvalidResponse)
	}
	if r.Action != ActionReview {
		t.Errorf("Action = %q, want %q (never auto on invalid_response)", r.Action, ActionReview)
	}
	if r.Verdict != "" || r.Confidence != 0 || r.Probabilities != nil {
		t.Errorf("expected zero-valued placeholders on invalid_response, got %+v", r)
	}
}

func TestValidateInput(t *testing.T) {
	if _, err := validateInput(VerifyInput{Claims: []string{"x"}, Evidence: "y"}); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]VerifyInput{
		"no claims":             {Claims: nil, Evidence: "y"},
		"blank claim":           {Claims: []string{"  "}, Evidence: "y"},
		"nil evidence":          {Claims: []string{"x"}, Evidence: nil},
		"empty string evidence": {Claims: []string{"x"}, Evidence: ""},
		"empty evidence array":  {Claims: []string{"x"}, Evidence: []any{}},
		"bad auto_accept":       {Claims: []string{"x"}, Evidence: "y", AutoAccept: 0.4},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_TooManyClaims(t *testing.T) {
	claims := make([]string, maxClaims+1)
	for i := range claims {
		claims[i] = "x"
	}
	if _, err := validateInput(VerifyInput{Claims: claims, Evidence: "y"}); err == nil {
		t.Error("expected error for too many claims")
	}
}
