package review

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
	"github.com/pyck-ai/jev-mcp/internal/tools/reviewcore"
)

// cliTestProvider builds a registry.DepsProvider pointed at a fake
// SystemOne server, mirroring newTestHandler's setup in review_test.go
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

// TestCLI_FlagsBasedInvocation exercises the per-field flag path,
// including Weights (a structured/nested-struct field, so its flag value
// is JSON-encoded -- see cliinput's package doc comment).
func TestCLI_FlagsBasedInvocation(t *testing.T) {
	srv := fakeServer(t, goodAnswers()) // reuses review_test.go's fake-OpenRouter helper
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--request", "add a widget",
		"--diff", "+func Widget() {}",
		"--weights", `{"correctness":0.4,"spec_match":0.3,"test_gap":0.15,"blast_radius":0.15}`,
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
	srv := fakeServer(t, goodAnswers())
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"request":"add a widget","diff":"+func Widget() {}"}`})
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
	srv := fakeServer(t, goodAnswers())
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--request", "add a widget", "--diff", "+func Widget() {}", "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got ReviewOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid ReviewOutput JSON: %v\n%s", err, out.String())
	}
	if got.Action != reviewcore.ActionAuto {
		t.Errorf("got %+v", got)
	}
}

// TestCLI_ExitCode_Escalate covers the exit-2 branch: a malformed rubric
// answer forces escalate (see reviewcore.ParseAssessment).
func TestCLI_ExitCode_Escalate(t *testing.T) {
	answersMap := goodAnswers()
	delete(answersMap, reviewcore.QuestionCorrectness)
	srv := fakeServer(t, answersMap)
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--request", "add a widget", "--diff", "+func Widget() {}"})
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

// TestCLI_ExitCode_Review covers the exit-1 branch: truncated input (over
// reviewcore.MaxRequestChars) blocks auto but doesn't force escalate.
func TestCLI_ExitCode_Review(t *testing.T) {
	srv := fakeServer(t, goodAnswers())
	defer srv.Close()

	longRequest := strings.Repeat("x", reviewcore.MaxRequestChars+1000)

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--request", longRequest, "--diff", "+func Widget() {}"})
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
	cmd.SetArgs([]string{"--diff", "d"}) // missing --request
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing request")
	}
}
