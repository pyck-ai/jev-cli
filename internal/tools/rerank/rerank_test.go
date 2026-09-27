package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *RerankHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewRerankHandler(client, cfg, tracker, auditLog)
}

func fakeServer(t *testing.T, answers map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00003},
		})
	}))
}

func TestRerankHandler_Handle_OK(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"cand0": json.RawMessage(`{"type":"noul","noul":0.2}`),
		"cand1": json.RawMessage(`{"type":"noul","noul":0.9}`),
		"cand2": json.RawMessage(`{"type":"noul","noul":0.5}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, RerankInput{
		Query: "q",
		Candidates: []Candidate{
			{ID: "a", Text: "x"}, {ID: "b", Text: "y"}, {ID: "c", Text: "z"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusOK {
		t.Fatalf("Status = %q, want ok", out.Status)
	}
	if len(out.Ranked) != 3 {
		t.Fatalf("len(Ranked) = %d, want 3", len(out.Ranked))
	}
	wantOrder := []string{"b", "c", "a"} // 0.9, 0.5, 0.2
	for i, id := range wantOrder {
		if out.Ranked[i].ID != id {
			t.Errorf("Ranked[%d].ID = %q, want %q", i, out.Ranked[i].ID, id)
		}
		if out.Ranked[i].Rank != i+1 {
			t.Errorf("Ranked[%d].Rank = %d, want %d", i, out.Ranked[i].Rank, i+1)
		}
	}
}

func TestRerankHandler_Handle_OneMalformedAnswerInvalidatesWholeCall(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"cand0": json.RawMessage(`{"type":"noul","noul":0.9}`),
		// cand1 missing -- should NOT silently become relevance 0.
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, RerankInput{
		Query:      "q",
		Candidates: []Candidate{{ID: "a", Text: "x"}, {ID: "b", Text: "y"}},
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Status != StatusInvalidResponse {
		t.Errorf("Status = %q, want invalid_response", out.Status)
	}
	if out.Ranked != nil {
		t.Errorf("expected no ranking at all when any candidate's answer is malformed, got %+v", out.Ranked)
	}
}

func TestValidateInput(t *testing.T) {
	valid := RerankInput{Query: "q", Candidates: []Candidate{{ID: "a", Text: "x"}}}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]RerankInput{
		"empty query":   {Query: " ", Candidates: []Candidate{{ID: "a", Text: "x"}}},
		"no candidates": {Query: "q", Candidates: nil},
		"duplicate id":  {Query: "q", Candidates: []Candidate{{ID: "a", Text: "x"}, {ID: "a", Text: "y"}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_AggregateCharCapRejectsRatherThanTruncates(t *testing.T) {
	big := strings.Repeat("x", maxAggregateChars/2+1)
	in := RerankInput{
		Query: "q",
		Candidates: []Candidate{
			{ID: "a", Text: big},
			{ID: "b", Text: big},
		},
	}
	err := validateInput(in)
	if err == nil {
		t.Fatal("expected error for aggregate text over cap")
	}
	if !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("expected an 'aggregate' error, got %v", err)
	}
}

func TestValidateInput_TooManyCandidates(t *testing.T) {
	cands := make([]Candidate, maxCandidates+1)
	for i := range cands {
		cands[i] = Candidate{ID: strconv.Itoa(i), Text: "x"}
	}
	if err := validateInput(RerankInput{Query: "q", Candidates: cands}); err == nil {
		t.Error("expected error for too many candidates")
	}
}
