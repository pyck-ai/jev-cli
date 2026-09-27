package rerank

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
// SystemOne server, mirroring newTestHandler's setup in rerank_test.go
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
// Restored automatically via t.Cleanup. The pointer -- not its pointed-to
// value -- must be captured before cmd.Execute() runs, since exitFunc is
// only actually invoked during that call.
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
		"cand0": json.RawMessage(`{"type":"noul","noul":0.9}`),
		"cand1": json.RawMessage(`{"type":"noul","noul":0.2}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--query", "q",
		"--candidates", `[{"id":"a","text":"x"},{"id":"b","text":"y"}]`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := new(int)
	old := exitFunc
	exitFunc = func(c int) { *codePtr = c }
	defer func() { exitFunc = old }()

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
	if !strings.Contains(out.String(), "ranked:") || !strings.Contains(out.String(), "status:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"cand0": json.RawMessage(`{"type":"noul","noul":0.5}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"query":"q","candidates":[{"id":"a","text":"x"}]}`})
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
	cmd.SetArgs([]string{"--json", `{"query":"q"}`, "--query", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--query") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"cand0": json.RawMessage(`{"type":"noul","noul":0.7}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--query", "q",
		"--candidates", `[{"id":"a","text":"x"}]`,
		"-o", "json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got RerankOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid RerankOutput JSON: %v\n%s", err, out.String())
	}
	if got.Status != StatusOK || len(got.Ranked) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeInvalidResponse(t *testing.T) {
	// cand1's answer is missing entirely -- fails the WHOLE call closed
	// (see package doc comment), which maps to exit 3, not a per-item
	// review tier.
	srv := fakeServer(t, map[string]json.RawMessage{
		"cand0": json.RawMessage(`{"type":"noul","noul":0.9}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--query", "q",
		"--candidates", `[{"id":"a","text":"x"},{"id":"b","text":"y"}]`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 3 {
		t.Errorf("exit code = %d, want 3", *code)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--candidates", `[{"id":"a","text":"x"}]`}) // missing --query
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing query")
	}
}
