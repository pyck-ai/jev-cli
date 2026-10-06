package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
	_ "github.com/pyck-ai/jev-cli/internal/tools/ask"
	_ "github.com/pyck-ai/jev-cli/internal/tools/check"
	_ "github.com/pyck-ai/jev-cli/internal/tools/classify"
	_ "github.com/pyck-ai/jev-cli/internal/tools/compare"
	"github.com/pyck-ai/jev-cli/internal/tools/decide"
	_ "github.com/pyck-ai/jev-cli/internal/tools/extract"
	_ "github.com/pyck-ai/jev-cli/internal/tools/gate"
	_ "github.com/pyck-ai/jev-cli/internal/tools/match"
	_ "github.com/pyck-ai/jev-cli/internal/tools/rerank"
	_ "github.com/pyck-ai/jev-cli/internal/tools/review"
	_ "github.com/pyck-ai/jev-cli/internal/tools/score"
	_ "github.com/pyck-ai/jev-cli/internal/tools/screen"
	_ "github.com/pyck-ai/jev-cli/internal/tools/verify"
)

// fake is a fake SystemOne server. A decide request answers with the
// candidate named by its decision text ("pick:<id>"), sleeping delay(decision)
// first; a check request answers noul 0.5 (review) when its context says
// "uncertain", else 0.9 (auto).
type fake struct {
	srv      *httptest.Server
	calls    atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
	delay    func(decision string) time.Duration
	cost     float64
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{cost: 0.00002}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			p := f.peak.Load()
			if n <= p || f.peak.CompareAndSwap(p, n) {
				break
			}
		}
		var req openrouter.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		var decision, checkCtx string
		switch st := req.State.(type) {
		case map[string]any:
			decision, _ = st["decision"].(string)
		case string:
			checkCtx = st
		}
		if f.delay != nil {
			time.Sleep(f.delay(decision))
		}
		answers := map[string]json.RawMessage{}
		for k := range req.Questions {
			switch {
			case k == "decision":
				id := strings.TrimPrefix(decision, "pick:")
				answers[k] = json.RawMessage(`{"type":"choice","choice":"` + id + `","confidence":0.9,"probabilities":{"` + id + `":0.9}}`)
			default:
				p := "0.9"
				if strings.Contains(checkCtx, "uncertain") {
					p = "0.5"
				}
				answers[k] = json.RawMessage(`{"type":"noul","noul":` + p + `}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openrouter.Response{Answers: answers, Model: "m", Usage: &openrouter.Usage{Cost: f.cost}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newDeps(t *testing.T, f *fake, cfg config.Config) (*registry.Deps, string) {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", f.srv.URL, openrouter.RetryPolicy{MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 5})
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	return &registry.Deps{
		Client: client, Config: cfg,
		Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession),
		Audit:  audit.NewLogger(auditPath),
	}, auditPath
}

func decideItem(id, pick string) Item {
	return Item{ID: id, Tool: "decide", Input: map[string]any{
		"decision": "pick:" + pick, "evidence": "e", "priorities": "p",
		"candidates": []any{
			map[string]any{"id": "a", "description": "A"},
			map[string]any{"id": "b", "description": "B"},
			map[string]any{"id": "c", "description": "C"},
		},
	}}
}

func checkItem(id, context string) Item {
	return Item{ID: id, Tool: "check", Input: map[string]any{"context": context, "propositions": []any{"x is fine"}}}
}

func TestRun_OrderPreservedUnderConcurrency(t *testing.T) {
	f := newFake(t)
	// Earlier items are slower, so completion order is the reverse of input order.
	f.delay = func(d string) time.Duration {
		switch d {
		case "pick:a":
			return 120 * time.Millisecond
		case "pick:b":
			return 60 * time.Millisecond
		}
		return 0
	}
	deps, _ := newDeps(t, f, config.Default())
	out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{
		Items: []Item{decideItem("first", "a"), decideItem("second", "b"), decideItem("third", "c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ id, sel string }{{"first", "a"}, {"second", "b"}, {"third", "c"}} {
		r := out.Results[i]
		if r.Index != i || r.ID != want.id || r.Tool != "decide" || r.Status != StatusOK {
			t.Fatalf("results[%d] = %+v", i, r)
		}
		if got := r.Output.(decide.DecideOutput).Recommendation.Selected; got != want.sel {
			t.Errorf("results[%d] selected = %q, want %q", i, got, want.sel)
		}
	}
	if out.Summary.Items != 3 || out.Summary.OK != 3 || out.Summary.Error != 0 || out.Summary.ExitCode != 0 {
		t.Errorf("summary = %+v", out.Summary)
	}
	if out.Summary.CostUSD == nil || *out.Summary.CostUSD < 0.00005 {
		t.Errorf("summary cost = %v, want ~0.00006", out.Summary.CostUSD)
	}
}

func TestRun_PerItemIsolation(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	bad := decideItem("bad", "a")
	bad.Input["candidates"] = []any{ // reserved id: preflight validation error
		map[string]any{"id": "none", "description": "N"}, map[string]any{"id": "b", "description": "B"},
	}
	unknownField := decideItem("typo", "a")
	unknownField.Input["bogus"] = 1
	out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{
		Items: []Item{decideItem("ok1", "a"), bad, unknownField, checkItem("ok2", "fine")},
	})
	if err != nil {
		t.Fatalf("batch itself must not fail: %v", err)
	}
	wantStatus := []string{StatusOK, StatusError, StatusError, StatusOK}
	for i, w := range wantStatus {
		if out.Results[i].Status != w {
			t.Errorf("results[%d].status = %q, want %q (%+v)", i, out.Results[i].Status, w, out.Results[i])
		}
	}
	if !strings.Contains(out.Results[1].Error, "reserved") || out.Results[1].ExitCode != 3 || out.Results[1].Output != nil {
		t.Errorf("results[1] = %+v", out.Results[1])
	}
	if !strings.Contains(out.Results[2].Error, "bogus") && !strings.Contains(out.Results[2].Error, "validating") {
		t.Errorf("results[2].error = %q, want unknown-field validation error", out.Results[2].Error)
	}
	if out.Summary.OK != 2 || out.Summary.Error != 2 || out.Summary.ExitCode != 3 {
		t.Errorf("summary = %+v", out.Summary)
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("server calls = %d, want 2 (invalid items must not reach the wire)", got)
	}
}

func TestRun_ConcurrencyBound(t *testing.T) {
	for _, tc := range []struct{ conc, wantPeak int }{{0, DefaultConcurrency}, {2, 2}, {1, 1}} {
		f := newFake(t)
		f.delay = func(string) time.Duration { return 30 * time.Millisecond }
		deps, _ := newDeps(t, f, config.Default())
		items := make([]Item, 12)
		for i := range items {
			items[i] = decideItem("", "a")
		}
		out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: items, MaxConcurrency: tc.conc})
		if err != nil {
			t.Fatal(err)
		}
		if out.Summary.OK != 12 {
			t.Fatalf("summary = %+v", out.Summary)
		}
		if got := f.peak.Load(); got > int64(tc.wantPeak) {
			t.Errorf("max_concurrency=%d: peak in-flight = %d, want <= %d", tc.conc, got, tc.wantPeak)
		} else if got < int64(tc.wantPeak) {
			t.Errorf("max_concurrency=%d: peak in-flight = %d, want it to reach %d", tc.conc, got, tc.wantPeak)
		}
	}
}

func TestRun_SessionBudgetStopsLaunches(t *testing.T) {
	t.Run("exhausted before start: no wire calls", func(t *testing.T) {
		f := newFake(t)
		cfg := config.Default()
		cfg.Budget.MaxUSDPerSession = 0.01
		deps, _ := newDeps(t, f, cfg)
		deps.Budget.Add(0.01)
		out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: []Item{decideItem("", "a"), checkItem("", "x")}})
		if err != nil {
			t.Fatal(err)
		}
		if out.Summary.Error != 2 || out.Summary.ExitCode != 3 || f.calls.Load() != 0 {
			t.Errorf("summary = %+v, calls = %d", out.Summary, f.calls.Load())
		}
		if !strings.Contains(out.Results[0].Error, "session budget exhausted") {
			t.Errorf("error = %q", out.Results[0].Error)
		}
	})
	t.Run("exhausted mid-batch: later items refused", func(t *testing.T) {
		f := newFake(t)
		f.cost = 0.01
		cfg := config.Default()
		cfg.Budget.MaxUSDPerSession = 0.01
		deps, _ := newDeps(t, f, cfg)
		out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{
			Items:          []Item{decideItem("", "a"), decideItem("", "a"), decideItem("", "a")},
			MaxConcurrency: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Summary.OK != 1 || out.Summary.Error != 2 || f.calls.Load() != 1 {
			t.Errorf("summary = %+v, calls = %d", out.Summary, f.calls.Load())
		}
		if out.Results[0].Status != StatusOK || out.Results[1].Status != StatusError || out.Results[2].Status != StatusError {
			t.Errorf("statuses = %s %s %s", out.Results[0].Status, out.Results[1].Status, out.Results[2].Status)
		}
	})
}

func TestRun_ExitCodeIsMax(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	run := func(items ...Item) BatchOutput {
		out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: items})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run(decideItem("", "a"), checkItem("", "fine"))
	if out.Summary.ExitCode != 0 || out.Results[0].ExitCode != 0 || out.Results[1].ExitCode != 0 {
		t.Errorf("all ok: %+v", out)
	}
	out = run(decideItem("", "a"), checkItem("", "uncertain"))
	if out.Results[1].ExitCode != 1 || out.Summary.ExitCode != 1 || out.Summary.Error != 0 {
		t.Errorf("review item: %+v", out)
	}
	out = run(checkItem("", "uncertain"), Item{Tool: "decide", Input: map[string]any{}})
	if out.Results[1].ExitCode != 3 || out.Summary.ExitCode != 3 {
		t.Errorf("error item: %+v", out)
	}
}

func TestRun_MalformedBatch(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	many := make([]Item, MaxItems+1)
	for i := range many {
		many[i] = checkItem("", "x")
	}
	tests := []struct {
		name string
		in   BatchInput
		want string
	}{
		{"empty", BatchInput{}, "at least 1"},
		{"too many", BatchInput{Items: many}, "max is 32"},
		{"bad concurrency", BatchInput{Items: many[:1], MaxConcurrency: 9}, "max_concurrency"},
		{"missing tool", BatchInput{Items: []Item{{Input: map[string]any{}}}}, "tool is required"},
		{"unknown tool", BatchInput{Items: []Item{checkItem("", "x"), {Tool: "nope"}}}, `unknown tool "nope"`},
		{"nested batch", BatchInput{Items: []Item{{Tool: "batch"}}}, "nested"},
		{"nested batch jev_ prefix", BatchInput{Items: []Item{{Tool: "jev_batch"}}}, "nested"},
		{"doctor", BatchInput{Items: []Item{{Tool: "doctor"}}}, "doctor"},
		{"jev_doctor", BatchInput{Items: []Item{{Tool: "jev_doctor"}}}, "doctor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := NewBatchHandler(deps).run(context.Background(), tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if len(out.Results) != 0 {
				t.Errorf("malformed batch returned results: %+v", out.Results)
			}
		})
	}
	if f.calls.Load() != 0 {
		t.Errorf("malformed batches reached the wire: %d calls", f.calls.Load())
	}
}

func TestRun_ToolWithoutRunIsPerItemError(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	h := NewBatchHandler(deps)
	h.lookup = func(name string) (registry.Tool, bool) {
		if name == "legacy" {
			return registry.Tool{Name: "legacy", MCPName: "jev_legacy"}, true
		}
		return registry.Lookup(name)
	}
	out, err := h.run(context.Background(), BatchInput{Items: []Item{{Tool: "legacy", Input: map[string]any{}}, checkItem("", "fine")}})
	if err != nil {
		t.Fatalf("batch-level error: %v", err)
	}
	if out.Results[0].Status != StatusError || out.Results[0].Error != `tool "legacy" does not support batch yet` {
		t.Errorf("results[0] = %+v", out.Results[0])
	}
	if out.Results[1].Status != StatusOK {
		t.Errorf("results[1] = %+v", out.Results[1])
	}
}

// normalize drops latency_ms (it differs run to run) from a JSON document.
func normalize(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "latency_ms")
	out, _ := json.Marshal(m)
	return string(out)
}

func TestRun_ItemOutputEqualsDirectCall(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	it := decideItem("d", "b")
	it.Input["requirements"] = []any{"must be cheap"}

	raw, _ := json.Marshal(it.Input)
	var direct decide.DecideInput
	if err := json.Unmarshal(raw, &direct); err != nil {
		t.Fatal(err)
	}
	_, want, err := decide.NewDecideHandler(deps.Client, deps.Config, deps.Budget, deps.Audit).Handle(context.Background(), nil, direct)
	if err != nil {
		t.Fatal(err)
	}

	out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: []Item{it}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].Status != StatusOK {
		t.Fatalf("result = %+v", out.Results[0])
	}
	if got, w := normalize(t, out.Results[0].Output), normalize(t, want); got != w {
		t.Errorf("batch item output differs from direct call\n batch:  %s\n direct: %s", got, w)
	}
}

func TestRun_AuditKeepsItemLinesPlusSummary(t *testing.T) {
	f := newFake(t)
	deps, auditPath := newDeps(t, f, config.Default())
	bad := Item{Tool: "decide", Input: map[string]any{}}
	if _, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: []Item{decideItem("", "a"), checkItem("", "fine"), bad}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var summary audit.Entry
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		counts[e.Tool]++
		if e.Tool == ToolNameBatch {
			summary = e
		}
	}
	if counts["jev_decide"] != 1 || counts["jev_check"] != 1 || counts[ToolNameBatch] != 1 {
		t.Errorf("audit lines by tool = %v, want one each (the invalid item never reaches its tool's audit)", counts)
	}
	if summary.ItemCount != 3 || summary.InvalidCount != 1 || summary.Status != StatusError || summary.CostUSD == nil {
		t.Errorf("summary line = %+v", summary)
	}
}

func TestRun_SharedCtxReachesItems(t *testing.T) {
	// Items run with the caller's ctx (that is how a recording call_id is
	// shared): a cancelled ctx fails each item rather than being replaced.
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := NewBatchHandler(deps).run(ctx, BatchInput{Items: []Item{checkItem("", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].Status != StatusError {
		t.Errorf("result = %+v, want error from cancelled ctx", out.Results[0])
	}
}

// typeAwareServer answers every question by its type: noul 0.9, choice = the
// first criteria key (sorted), score = 1. Tool-specific parsing may still
// mark an answer invalid_response; the batch test only needs well-formed
// runs, not meaningful verdicts.
func typeAwareServer(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var req openrouter.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]json.RawMessage{}
		for k, q := range req.Questions {
			switch q.Type {
			case "noul":
				answers[k] = json.RawMessage(`{"type":"noul","noul":0.9}`)
			case "score":
				answers[k] = json.RawMessage(`{"type":"score","score":1,"confidence":0.9,"probabilities":{"0":0.05,"1":0.9,"2":0.05}}`)
			default:
				first := ""
				if m, ok := q.Criteria.(map[string]any); ok {
					for c := range m {
						if first == "" || c < first {
							first = c
						}
					}
				}
				answers[k] = json.RawMessage(`{"type":"choice","choice":"` + first + `","confidence":0.9,"probabilities":{"` + first + `":0.9}}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openrouter.Response{Answers: answers, Model: "m", Usage: &openrouter.Usage{Cost: 0.00001}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestRun_EveryToolIsBatchable runs one minimal valid item of every tool
// except doctor and batch through the real registry and requires a
// well-formed per-item result: never "does not support batch".
func TestRun_EveryToolIsBatchable(t *testing.T) {
	f := typeAwareServer(t)
	deps, _ := newDeps(t, f, config.Default())

	cands := []any{map[string]any{"id": "a", "text": "alpha"}, map[string]any{"id": "b", "text": "beta"}}
	review := map[string]any{"request": "add x", "diff": "+x"}
	inputs := map[string]map[string]any{
		"ask": {"state": "s", "questions": map[string]any{"q": map[string]any{
			"type": "noul", "instructions": "i", "criteria": map[string]any{"true": "t", "false": "f"}}}},
		"check":    {"propositions": []any{"x"}},
		"classify": {"items": []any{map[string]any{"id": "1", "text": "crash"}}, "classes": []any{map[string]any{"id": "bug", "description": "a bug"}, map[string]any{"id": "feature", "description": "a feature"}}},
		"compare":  {"passage_a": "a", "passage_b": "b"},
		"decide":   decideItem("", "a").Input,
		"extract":  {"document": "name: Bob", "fields": []any{map[string]any{"id": "name", "pattern": `name: \w+`, "description": "the name"}}},
		"gate":     {"request": "add x", "diff": "+x", "claims": []any{"x added"}, "evidence": []any{map[string]any{"id": "e", "text": "x added"}}},
		"match":    {"query": "q", "candidates": cands},
		"rerank":   {"query": "q", "candidates": cands},
		"review":   review,
		"score":    {"state": "s", "scale_min": 0, "scale_max": 2, "instructions": "0 bad 2 good"},
		"screen":   {"text": "hello"},
		"verify":   {"claims": []any{"x"}, "evidence": "x is true"},
	}

	var items []Item
	var names []string
	for _, tl := range registry.All() {
		if tl.Name == "doctor" || tl.Name == "batch" {
			continue
		}
		in, ok := inputs[tl.Name]
		if !ok {
			t.Errorf("no minimal input for registered tool %q: add one to this test", tl.Name)
			continue
		}
		items = append(items, Item{ID: tl.Name, Tool: tl.Name, Input: in})
		names = append(names, tl.Name)
	}
	if len(items) != len(inputs) {
		t.Fatalf("registered batchable tools = %v, want exactly the %d with inputs (blank imports missing?)", names, len(inputs))
	}
	out, err := NewBatchHandler(deps).run(context.Background(), BatchInput{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range out.Results {
		if r.Index != i || r.ID != items[i].ID || r.Tool != items[i].Tool {
			t.Errorf("results[%d] = %+v", i, r)
		}
		if strings.Contains(r.Error, "does not support batch") {
			t.Errorf("%s: %s", r.Tool, r.Error)
		}
		if r.Status != StatusOK || r.Output == nil {
			t.Errorf("%s: status %q error %q, want ok with output", r.Tool, r.Status, r.Error)
		}
	}
}
