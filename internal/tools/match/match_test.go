package match

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *MatchHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewMatchHandler(client, cfg, tracker, auditLog)
}

func fakeServer(t *testing.T, pick, exists string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		answers := map[string]json.RawMessage{}
		if pick != "" {
			answers["pick"] = json.RawMessage(pick)
		}
		if exists != "" {
			answers["exists"] = json.RawMessage(exists)
		}
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00002},
		})
	}))
}

func TestMatchHandler_Handle_OK(t *testing.T) {
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":0.7,"b":0.2,"c":0.1}}`,
		`{"type":"noul","noul":0.9}`,
	)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, MatchInput{
		Query: "what is the answer",
		Candidates: []Candidate{
			{ID: "a", Text: "the answer is 42"},
			{ID: "b", Text: "unrelated"},
			{ID: "c", Text: "also unrelated"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != StatusOK {
		t.Fatalf("Status = %q, want ok", out.Status)
	}
	if len(out.Top) != 3 || out.Top[0].ID != "a" || out.Top[0].Probability != 0.7 {
		t.Errorf("Top = %+v, want [a:0.7 ...] first", out.Top)
	}
	if out.Exists != 0.9 || out.ExistsVerdict != VerdictAnswered {
		t.Errorf("Exists/ExistsVerdict = %v/%q, want 0.9/answered", out.Exists, out.ExistsVerdict)
	}
}

func TestMatchHandler_Handle_TopKLimitsResults(t *testing.T) {
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":0.6,"b":0.3,"c":0.1}}`,
		`{"type":"noul","noul":0.2}`,
	)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, MatchInput{
		Query: "q",
		Candidates: []Candidate{
			{ID: "a", Text: "x"}, {ID: "b", Text: "y"}, {ID: "c", Text: "z"},
		},
		TopK: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Top) != 1 || out.Top[0].ID != "a" {
		t.Errorf("Top = %+v, want exactly [a]", out.Top)
	}
	if out.ExistsVerdict != VerdictAbsent {
		t.Errorf("ExistsVerdict = %q, want absent for exists=0.2", out.ExistsVerdict)
	}
}

func TestMatchHandler_Handle_MalformedAnswerFailsClosed(t *testing.T) {
	// "nonexistent" is not among the offered candidate ids (validOptions),
	// so this must be treated as a malformed/fail-closed answer, not a
	// literal JSON-parse failure.
	srv := fakeServer(t, `{"type":"choice","choice":"nonexistent","confidence":0.9,"probabilities":{"nonexistent":1}}`, `{"type":"noul","noul":0.9}`)
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, MatchInput{
		Query:      "q",
		Candidates: []Candidate{{ID: "a", Text: "x"}},
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	if out.Status != StatusInvalidResponse {
		t.Errorf("Status = %q, want invalid_response", out.Status)
	}
	if out.Top != nil || out.Exists != 0 || out.ExistsVerdict != "" {
		t.Errorf("expected zero-valued placeholders on invalid_response, got %+v", out)
	}
}

func TestValidateInput(t *testing.T) {
	valid := MatchInput{Query: "q", Candidates: []Candidate{{ID: "a", Text: "x"}}}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]MatchInput{
		"empty query":          {Query: "  ", Candidates: []Candidate{{ID: "a", Text: "x"}}},
		"no candidates":        {Query: "q", Candidates: nil},
		"empty candidate id":   {Query: "q", Candidates: []Candidate{{ID: "", Text: "x"}}},
		"duplicate id":         {Query: "q", Candidates: []Candidate{{ID: "a", Text: "x"}, {ID: "a", Text: "y"}}},
		"empty candidate text": {Query: "q", Candidates: []Candidate{{ID: "a", Text: " "}}},
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
	cands := make([]Candidate, maxCandidates+1)
	for i := range cands {
		cands[i] = Candidate{ID: strconv.Itoa(i), Text: "x"}
	}
	if err := validateInput(MatchInput{Query: "q", Candidates: cands}); err == nil {
		t.Error("expected error for too many candidates")
	} else if !strings.Contains(err.Error(), "too many") {
		t.Errorf("expected a 'too many candidates' error, got %v", err)
	}
}

func TestPerCandidateTextIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		q := req.Questions["pick"]
		criteria, ok := q.Criteria.(map[string]any)
		if !ok {
			t.Fatalf("expected pick criteria to decode as an object, got %T", q.Criteria)
		}
		text, _ := criteria["a"].(string)
		if len([]rune(text)) != maxCandidateTextChars {
			t.Errorf("candidate text length = %d, want exactly %d (truncated)", len([]rune(text)), maxCandidateTextChars)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"pick":   json.RawMessage(`{"type":"choice","choice":"a","confidence":0.5,"probabilities":{"a":1}}`),
				"exists": json.RawMessage(`{"type":"noul","noul":0.5}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	longText := strings.Repeat("x", maxCandidateTextChars+500)
	_, _, err := h.Handle(context.Background(), nil, MatchInput{
		Query:      "q",
		Candidates: []Candidate{{ID: "a", Text: longText}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
