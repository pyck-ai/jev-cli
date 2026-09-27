package match

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
// SystemOne server, mirroring newTestHandler's setup in match_test.go
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
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":0.7,"b":0.3}}`,
		`{"type":"noul","noul":0.9}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--query", "what is the answer",
		"--candidates", `[{"id":"a","text":"the answer is 42"},{"id":"b","text":"unrelated"}]`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
	if !strings.Contains(out.String(), "exists_verdict:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":1}}`,
		`{"type":"noul","noul":0.9}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"query":"q","candidates":[{"id":"a","text":"x"}]}`})
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
	cmd.SetArgs([]string{"--json", `{"query":"q","candidates":[{"id":"a","text":"x"}]}`, "--query", "z"})
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
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":1}}`,
		`{"type":"noul","noul":0.9}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--query", "q", "--candidates", `[{"id":"a","text":"x"}]`, "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got MatchOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid MatchOutput JSON: %v\n%s", err, out.String())
	}
	if got.Status != StatusOK || got.ExistsVerdict != VerdictAnswered {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodePartial(t *testing.T) {
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":1}}`,
		`{"type":"noul","noul":0.5}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--query", "q", "--candidates", `[{"id":"a","text":"x"}]`})
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

func TestCLI_ExitCodeAbsent(t *testing.T) {
	srv := fakeServer(t,
		`{"type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":1}}`,
		`{"type":"noul","noul":0.1}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--query", "q", "--candidates", `[{"id":"a","text":"x"}]`})
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

func TestCLI_ExitCodeInvalidResponse(t *testing.T) {
	// "nonexistent" is not among the offered candidate ids -> fails
	// closed to Status == invalid_response (see
	// TestMatchHandler_Handle_MalformedAnswerFailsClosed in match_test.go),
	// which exitCode maps to 3 regardless of ExistsVerdict (empty here).
	srv := fakeServer(t,
		`{"type":"choice","choice":"nonexistent","confidence":0.9,"probabilities":{"nonexistent":1}}`,
		`{"type":"noul","noul":0.9}`,
	)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--query", "q", "--candidates", `[{"id":"a","text":"x"}]`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 3 {
		t.Errorf("exit code = %d, want 3", *codePtr)
	}
}
