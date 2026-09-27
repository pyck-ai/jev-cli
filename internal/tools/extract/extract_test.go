package extract

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *ExtractHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewExtractHandler(client, cfg, tracker, auditLog)
}

func TestExtractHandler_Handle_ZeroMatchesMakesNoAPICallAtAll(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "hello world, nothing interesting here",
		Fields:   []Field{{ID: "email", Pattern: `[\w.]+@[\w.]+`, Description: "an email address"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected NO API call at all when every field has zero regex matches")
	}
	if len(out.Fields) != 1 || out.Fields[0].Status != StatusNotFound {
		t.Errorf("Fields = %+v, want a single not_found result", out.Fields)
	}
	if out.Model != "" || out.Usage != nil {
		t.Errorf("expected no Model/Usage when no API call was made, got Model=%q Usage=%+v", out.Model, out.Usage)
	}
}

func TestExtractHandler_Handle_InvalidPatternNoAPICallForThatField(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "hello",
		Fields:   []Field{{ID: "bad", Pattern: `(unclosed`, Description: "a broken pattern"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Fields[0].Status != StatusInvalidPattern {
		t.Errorf("Status = %q, want invalid_pattern", out.Fields[0].Status)
	}
}

func TestExtractHandler_Handle_PicksCandidateAndAutoAccepts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		q, ok := req.Questions["email"]
		if !ok {
			t.Fatalf("expected a question keyed by field id \"email\", got %v", req.Questions)
		}
		criteria, ok := q.Criteria.(map[string]any)
		if !ok {
			t.Fatalf("expected criteria to decode as an object, got %T", q.Criteria)
		}
		if _, ok := criteria["a@b.com"]; !ok {
			t.Errorf("expected the matched substring itself to be a criteria key, got %v", criteria)
		}
		if _, ok := criteria[noneOfThem]; !ok {
			t.Errorf("expected a %q criteria key, got %v", noneOfThem, criteria)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"email": json.RawMessage(`{"type":"choice","choice":"a@b.com","confidence":0.95,"probabilities":{"a@b.com":0.95,"none_of_them":0.05}}`),
			},
			Model: "typesafe/jev-1.13-20260917",
			Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "contact me at a@b.com please",
		Fields:   []Field{{ID: "email", Pattern: `[\w.]+@[\w.]+`, Description: "an email address"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Fields[0]
	if r.Value == nil || *r.Value != "a@b.com" {
		t.Fatalf("Value = %v, want a@b.com", r.Value)
	}
	if r.Status != StatusAuto {
		t.Errorf("Status = %q, want auto", r.Status)
	}
	if r.CandidatesConsidered != 1 || r.CandidatesTruncated {
		t.Errorf("CandidatesConsidered/Truncated = %d/%v, want 1/false", r.CandidatesConsidered, r.CandidatesTruncated)
	}
}

func TestExtractHandler_Handle_NoneOfThemIsReviewNotFabricated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"` + noneOfThem + `","confidence":0.9,"probabilities":{}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "call 12345 or 67890",
		Fields:   []Field{{ID: "num", Pattern: `\d+`, Description: "the relevant number"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Fields[0]
	if r.Value != nil {
		t.Errorf("Value = %v, want nil when the model picked none_of_them", *r.Value)
	}
	if r.Status != StatusReview {
		t.Errorf("Status = %q, want review", r.Status)
	}
}

func TestExtractHandler_Handle_MalformedAnswerFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{}, // "num" missing entirely
			Model:   "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "call 12345",
		Fields:   []Field{{ID: "num", Pattern: `\d+`, Description: "the relevant number"}},
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	r := out.Fields[0]
	if r.Status != StatusInvalidResponse || r.Value != nil {
		t.Errorf("got status=%q value=%v, want invalid_response/nil", r.Status, r.Value)
	}
}

func TestExtractHandler_Handle_MixedFieldsOnlyCallsModelForPendingOnes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Questions) != 1 {
			t.Fatalf("expected exactly 1 question (only the field with matches), got %d: %v", len(req.Questions), req.Questions)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"123","confidence":0.9,"probabilities":{"123":0.9,"none_of_them":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ExtractInput{
		Document: "the code is 123",
		Fields: []Field{
			{ID: "num", Pattern: `\d+`, Description: "the code"},
			{ID: "email", Pattern: `[\w.]+@[\w.]+`, Description: "an email, none present"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Fields) != 2 {
		t.Fatalf("len(Fields) = %d, want 2", len(out.Fields))
	}
	byID := map[string]FieldResult{}
	for _, f := range out.Fields {
		byID[f.ID] = f
	}
	if byID["num"].Status != StatusAuto || byID["num"].Value == nil || *byID["num"].Value != "123" {
		t.Errorf("num = %+v", byID["num"])
	}
	if byID["email"].Status != StatusNotFound {
		t.Errorf("email = %+v, want not_found", byID["email"])
	}
}

func TestFindCandidates_DedupsAndCapsAndReportsTruncation(t *testing.T) {
	re := regexpMustCompile(t, `\d+`)
	candidates, truncated, timedOut := findCandidates(re, "1 2 1 3 1 4 1 5 1 6 1 7 1 8 1 9 1 10 1 11 1 12 1", 5, time.Second)
	if timedOut {
		t.Fatal("did not expect a timeout")
	}
	if !truncated {
		t.Error("expected truncated=true (more than 5 distinct-position matches exist)")
	}
	if len(candidates) == 0 {
		t.Error("expected at least one candidate")
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			t.Errorf("expected deduplicated candidates, saw %q twice", c)
		}
		seen[c] = true
	}
}

func TestFindCandidates_TimesOut(t *testing.T) {
	re := regexpMustCompile(t, `.*`)
	_, _, timedOut := findCandidates(re, "anything", 5, 1*time.Nanosecond)
	if !timedOut {
		t.Error("expected an effectively-zero timeout to report timedOut=true")
	}
}

func TestValidateInput(t *testing.T) {
	valid := ExtractInput{Document: "d", Fields: []Field{{ID: "f", Pattern: "x", Description: "d"}}}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]ExtractInput{
		"empty document":     {Document: " ", Fields: []Field{{ID: "f", Pattern: "x", Description: "d"}}},
		"no fields":          {Document: "d", Fields: nil},
		"empty field id":     {Document: "d", Fields: []Field{{ID: "", Pattern: "x", Description: "d"}}},
		"duplicate field id": {Document: "d", Fields: []Field{{ID: "f", Pattern: "x", Description: "d"}, {ID: "f", Pattern: "y", Description: "d"}}},
		"empty pattern":      {Document: "d", Fields: []Field{{ID: "f", Pattern: "", Description: "d"}}},
		"empty description":  {Document: "d", Fields: []Field{{ID: "f", Pattern: "x", Description: ""}}},
		"bad auto_accept":    {Document: "d", Fields: []Field{{ID: "f", Pattern: "x", Description: "d"}}, AutoAccept: 0.5},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateInput(in); err == nil {
				t.Errorf("expected error for case %q", name)
			}
		})
	}
}

func TestValidateInput_TooManyFields(t *testing.T) {
	fields := make([]Field, maxFields+1)
	for i := range fields {
		fields[i] = Field{ID: string(rune('a')) + string(rune(i)), Pattern: "x", Description: "d"}
	}
	if err := validateInput(ExtractInput{Document: "d", Fields: fields}); err == nil {
		t.Error("expected error for too many fields")
	}
}

func regexpMustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("regexp.Compile(%q): %v", pattern, err)
	}
	return re
}
