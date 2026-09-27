package gate

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
// SystemOne server, mirroring newTestHandler's setup in gate_test.go but
// exposed as the lazy provider shape the CLI adapter expects.
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

// TestCLI_FlagsBasedInvocation_AllAuto exercises the per-field flag path,
// including Claims (structured slice-of-string) and Evidence (structured
// slice-of-struct), both of which take JSON-encoded flag values -- see
// cliinput's package doc comment.
func TestCLI_FlagsBasedInvocation_AllAuto(t *testing.T) {
	answersMap := goodReviewAnswers() // reuses gate_test.go's fake-OpenRouter helpers
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--request", "add a widget",
		"--diff", "+func Widget() {}",
		"--claims", `["the widget is thread-safe"]`,
		"--evidence", `[{"id":"e1","text":"Widget has no shared mutable state."}]`,
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
		t.Errorf("exit code = %d, want 0 (auto)", *codePtr)
	}
	if !strings.Contains(out.String(), "action:") || !strings.Contains(out.String(), "auto") {
		t.Errorf("text output missing expected action:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"request":"add a widget","diff":"+func Widget() {}","claims":["the widget is thread-safe"],"evidence":[{"id":"e1","text":"Widget has no shared mutable state."}]}`})
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
}

func TestCLI_JSONFlagConflictsWithFieldFlag(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--json", `{"request":"r"}`, "--request", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--request") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"supports","confidence":0.9,"probabilities":{"supports":0.9,"contradicts":0.05,"says_nothing":0.05}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--request", "add a widget",
		"--diff", "+func Widget() {}",
		"--claims", `["the widget is thread-safe"]`,
		"--evidence", `[{"id":"e1","text":"Widget has no shared mutable state."}]`,
		"-o", "json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got GateOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid GateOutput JSON: %v\n%s", err, out.String())
	}
	if got.Action != ActionAuto {
		t.Errorf("got %+v", got)
	}
}

// TestCLI_ExitCode_Escalate covers the exit-2 branch: a confidently
// contradicted claim forces escalate even though the review half is
// healthy (see computeAction).
func TestCLI_ExitCode_Escalate(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"contradicts","confidence":0.95,"probabilities":{"supports":0.02,"contradicts":0.95,"says_nothing":0.03}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--request", "add a widget",
		"--diff", "+func Widget() {}",
		"--claims", `["the widget is thread-safe"]`,
		"--evidence", `[{"id":"e1","text":"Widget has no shared mutable state."}]`,
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
	if *codePtr != 2 {
		t.Errorf("exit code = %d, want 2 (escalate)", *codePtr)
	}
}

// TestCLI_ExitCode_Review covers the exit-1 branch: a low-confidence,
// non-contradicting claim with an otherwise-healthy review is "review",
// not "escalate".
func TestCLI_ExitCode_Review(t *testing.T) {
	answersMap := goodReviewAnswers()
	answersMap["claim0"] = json.RawMessage(`{"type":"choice","choice":"says_nothing","confidence":0.5,"probabilities":{"supports":0.3,"contradicts":0.2,"says_nothing":0.5}}`)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--request", "add a widget",
		"--diff", "+func Widget() {}",
		"--claims", `["the widget is thread-safe"]`,
		"--evidence", `[{"id":"e1","text":"Widget has no shared mutable state."}]`,
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
	if *codePtr != 1 {
		t.Errorf("exit code = %d, want 1 (review)", *codePtr)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--request", "r", "--diff", "d"}) // missing --claims/--evidence
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing claims/evidence")
	}
}
