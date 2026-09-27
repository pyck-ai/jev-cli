package classify

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

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) *ClassifyHandler {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewClassifyHandler(client, cfg, tracker, auditLog)
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

func testClasses() []Class {
	return []Class{{ID: "bug", Description: "a defect report"}, {ID: "feature", Description: "a feature request"}}
}

func TestClassifyHandler_Handle_OK_AutoDecision(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.95,"probabilities":{"bug":0.95,"feature":0.05}}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ClassifyInput{
		Items:   []Item{{ID: "i1", Text: "it crashes on startup"}},
		Classes: testClasses(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Results[0]
	if r.ID != "i1" || r.Classification != "bug" {
		t.Errorf("Results[0] = %+v", r)
	}
	if diff := r.Margin - 0.9; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("Margin = %v, want ~0.9 (0.95-0.05)", r.Margin)
	}
	if r.Decision != DecisionAuto {
		t.Errorf("Decision = %q, want auto (confidence 0.95 >= 0.85, margin 0.9 >= 0.5)", r.Decision)
	}
}

func TestClassifyHandler_Handle_LowMarginIsReview(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.9,"probabilities":{"bug":0.55,"feature":0.45}}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ClassifyInput{
		Items:   []Item{{ID: "i1", Text: "x"}},
		Classes: testClasses(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := out.Results[0]
	if diff := r.Margin - 0.1; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("test setup: Margin = %v, want ~0.1", r.Margin)
	}
	if r.Decision != DecisionReview {
		t.Errorf("Decision = %q, want review (margin 0.1 < default minimum_margin 0.5)", r.Decision)
	}
}

func TestClassifyHandler_Handle_MalformedAnswerFailsClosed(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"not_a_class","confidence":0.9}`),
	})
	defer srv.Close()

	h := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, ClassifyInput{
		Items:   []Item{{ID: "i1", Text: "x"}},
		Classes: testClasses(),
	})
	if err != nil {
		t.Fatalf("expected a structured result, not a Go error: %v", err)
	}
	r := out.Results[0]
	if r.Status != StatusInvalidResponse || r.Decision != DecisionReview {
		t.Errorf("got status=%q decision=%q, want invalid_response/review", r.Status, r.Decision)
	}
	if r.Classification != "" || r.Margin != 0 || r.Probabilities != nil {
		t.Errorf("expected zero-valued placeholders on invalid_response, got %+v", r)
	}
}

func TestValidateInput(t *testing.T) {
	valid := ClassifyInput{Items: []Item{{ID: "i1", Text: "x"}}, Classes: testClasses()}
	if err := validateInput(valid); err != nil {
		t.Errorf("expected valid input to pass, got %v", err)
	}
	cases := map[string]ClassifyInput{
		"no items":           {Items: nil, Classes: testClasses()},
		"no classes":         {Items: []Item{{ID: "i1", Text: "x"}}, Classes: nil},
		"duplicate item id":  {Items: []Item{{ID: "i1", Text: "x"}, {ID: "i1", Text: "y"}}, Classes: testClasses()},
		"duplicate class id": {Items: []Item{{ID: "i1", Text: "x"}}, Classes: []Class{{ID: "a", Description: "d"}, {ID: "a", Description: "d2"}}},
		"bad auto_accept":    {Items: []Item{{ID: "i1", Text: "x"}}, Classes: testClasses(), AutoAccept: 0.5},
		"bad minimum_margin": {Items: []Item{{ID: "i1", Text: "x"}}, Classes: testClasses(), MinimumMargin: 1.5},
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
// validation error (see classify.go's jsonschema tags and
// validateInput): an agent guessing the shape of a nested field like
// items[].id or classes[].description must get an error that shows the
// expected shape, not just "what's wrong". See internal/tools/ask's
// identically-named test for the precedent this follows.
func TestValidateInput_ErrorsExplainTheExpectedShape(t *testing.T) {
	cases := map[string]struct {
		in   ClassifyInput
		want string
	}{
		"no items":   {ClassifyInput{Items: nil, Classes: testClasses()}, `{"id": "i1", "text": "..."}`},
		"no classes": {ClassifyInput{Items: []Item{{ID: "i1", Text: "x"}}, Classes: nil}, `{"id": "bug", "description": "a defect report"}`},
		"empty item id": {
			ClassifyInput{Items: []Item{{ID: "", Text: "x"}}, Classes: testClasses()},
			"unique non-empty string id",
		},
		"empty class description": {
			ClassifyInput{Items: []Item{{ID: "i1", Text: "x"}}, Classes: []Class{{ID: "a", Description: ""}}},
			"tell it apart from the others",
		},
		"duplicate item id": {
			ClassifyInput{Items: []Item{{ID: "i1", Text: "x"}, {ID: "i1", Text: "y"}}, Classes: testClasses()},
			"already used by another item",
		},
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

func TestValidateInput_ItemClassBudgetCap(t *testing.T) {
	items := make([]Item, 64)
	for i := range items {
		items[i] = Item{ID: string(rune('a'+i%26)) + string(rune(i)), Text: "x"}
	}
	classes := make([]Class, 200) // 64*200 = 12800 > 8000
	for i := range classes {
		classes[i] = Class{ID: string(rune('a'+i%26)) + string(rune(i)), Description: "d"}
	}
	if err := validateInput(ClassifyInput{Items: items, Classes: classes}); err == nil {
		t.Error("expected error for items*classes over the 8000 budget")
	}
}
