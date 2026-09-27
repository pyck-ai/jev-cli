package verify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// cliTestProvider builds a registry.DepsProvider pointed at a fake
// SystemOne server, mirroring newTestHandler's setup in verify_test.go
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

// withRecordedExit swaps exitFunc for the duration of a test with a stub
// that records the code it was called with into the returned *int
// (-1 until exitFunc is actually invoked), restoring the original
// exitFunc via t.Cleanup.
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
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`),
	}, nil)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--claims", `["the sky is blue"]`, "--evidence", `"the sky is blue"`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
	if !strings.Contains(out.String(), "results:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`),
	}, nil)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"claims":["x"],"evidence":"y"}`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
}

func TestCLI_JSONFlagConflictsWithFieldFlag(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--json", `{"claims":["x"],"evidence":"y"}`, "--claims", `["z"]`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--claims") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`),
	}, nil)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--claims", `["x"]`, "--evidence", `"y"`, "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got VerifyOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid VerifyOutput JSON: %v\n%s", err, out.String())
	}
	if len(got.Results) != 1 || got.Results[0].Status != StatusOK || got.Results[0].Verdict != VerdictSupports {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeReview(t *testing.T) {
	// Confidence 0.55 is below the default auto_accept (0.8) -> Action
	// review, but Verdict is "supports" (not "contradicts") -> exit 1,
	// not 2.
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.55,"probabilities":{"supports":0.55,"contradicts":0.25,"says_nothing":0.2}}`),
	}, nil)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--claims", `["x"]`, "--evidence", `"y"`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 1 {
		t.Errorf("exit code = %d, want 1", *codePtr)
	}
}

func TestCLI_ExitCodeContradicts(t *testing.T) {
	// A confident "contradicts" verdict (Action == auto) must still map
	// to exit 2: the verdict check takes priority over the action check
	// (see exitCode's doc comment).
	srv := fakeServer(t, map[string]json.RawMessage{
		"c0": json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.95,"probabilities":{"supports":0.02,"contradicts":0.95,"says_nothing":0.03}}`),
	}, nil)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--claims", `["x"]`, "--evidence", `"y"`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 2 {
		t.Errorf("exit code = %d, want 2", *codePtr)
	}
}
