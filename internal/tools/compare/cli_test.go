package compare

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// cliTestProvider builds a registry.DepsProvider pointed at a fake
// SystemOne server, mirroring newTestHandler's setup in compare_test.go
// but exposed as the lazy provider shape the CLI adapter expects.
func cliTestProvider(t *testing.T, serverURL string) registry.DepsProvider {
	t.Helper()
	cfg := config.Default()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	return func() *registry.Deps {
		return &registry.Deps{
			Client: client,
			Config: cfg,
			Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession),
			Audit:  audit.NewLogger(t.TempDir() + "/audit.jsonl"),
		}
	}
}

// withRecordedExit swaps exitFunc for the duration of a test, returning
// a pointer to an int (pre-set to -1, an exit code this package never
// produces) that the swapped-in exitFunc updates in place when called.
// Restored automatically via t.Cleanup.
func withRecordedExit(t *testing.T) *int {
	t.Helper()
	code := new(int)
	*code = -1
	old := exitFunc
	exitFunc = func(c int) { *code = c }
	t.Cleanup(func() { exitFunc = old })
	return code
}

func TestCLI_FlagsBasedInvocation(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.9,"probabilities":{"same_fact":0.05,"contradicts":0.9,"different_facts":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--passage-a", "the sky is blue", "--passage-b", "the sky is red"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 0 {
		t.Errorf("exit code = %d, want 0", *code)
	}
	if !strings.Contains(out.String(), "overall:") || !strings.Contains(out.String(), "relation:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"same_fact","confidence":0.9,"probabilities":{"same_fact":0.9,"contradicts":0.05,"different_facts":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"passage_a":"a","passage_b":"b"}`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 0 {
		t.Errorf("exit code = %d, want 0", *code)
	}
}

func TestCLI_JSONFlagConflictsWithFieldFlag(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--json", `{"passage_a":"a"}`, "--passage-a", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--passage-a") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"same_fact","confidence":0.9,"probabilities":{"same_fact":0.9,"contradicts":0.05,"different_facts":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--passage-a", "a", "--passage-b", "b", "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got CompareOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid CompareOutput JSON: %v\n%s", err, out.String())
	}
	if got.Overall.Relation != RelationSameFact || got.Overall.Decision != DecisionAuto {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeReviewOnOverall(t *testing.T) {
	// overall's answer is missing entirely -- Status=invalid_response and
	// Decision forced to "review", which maps to exit 1.
	srv := fakeServer(t, map[string]json.RawMessage{})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--passage-a", "a", "--passage-b", "b"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 1 {
		t.Errorf("exit code = %d, want 1", *code)
	}
}

func TestCLI_ExitCodeReviewOnAspect(t *testing.T) {
	// overall is fine (auto), but the one aspect's confidence is below
	// the default auto_accept (0.8) -- Decision=review for that aspect
	// only, still exit 1.
	srv := fakeServer(t, map[string]json.RawMessage{
		"overall": json.RawMessage(`{"type":"choice","choice":"same_fact","confidence":0.9,"probabilities":{"same_fact":0.9,"contradicts":0.05,"different_facts":0.05}}`),
		"aspect0": json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.6,"probabilities":{"same_fact":0.2,"contradicts":0.6,"different_facts":0.2}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--passage-a", "a", "--passage-b", "b", "--aspects", `["pricing"]`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 1 {
		t.Errorf("exit code = %d, want 1 (aspect below auto_accept)", *code)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--passage-b", "b"}) // missing --passage-a
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing passage_a")
	}
}
