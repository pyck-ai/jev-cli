package ask

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
// SystemOne server, mirroring newTestHandler's setup in ask_test.go but
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

// TestCLI_FlagsBasedInvocation exercises the per-field flag path. Both
// State (an `any` field, JSON-kind Interface) and Questions (a map) are
// structured fields, so their flag values must be JSON-encoded -- see
// cliinput's package doc comment and TestCLI_StateFlagRequiresJSON /
// TestCLI_StateFlagAcceptsJSONString below for exactly what that means
// for --state specifically.
func TestCLI_FlagsBasedInvocation(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{ // reuses ask_test.go's fake-OpenRouter helper
		"q_noul": json.RawMessage(`{"type":"noul","noul":0.8}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--state", `"some state"`,
		"--questions", `{"q_noul":{"type":"noul","instructions":"is it?","criteria":{"true":"yes","false":"no"}}}`,
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
	if !strings.Contains(out.String(), "answers:") {
		t.Errorf("text output missing expected answers:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"q": json.RawMessage(`{"type":"noul","noul":0.5}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"state":"s","questions":{"q":{"type":"noul","instructions":"i","criteria":{"true":"t","false":"f"}}}}`})
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
	cmd.SetArgs([]string{"--json", `{"state":"x"}`, "--state", `"y"`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--state") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"q_noul": json.RawMessage(`{"type":"noul","noul":0.8}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--state", `"some state"`,
		"--questions", `{"q_noul":{"type":"noul","instructions":"is it?","criteria":{"true":"yes","false":"no"}}}`,
		"-o", "json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got AskOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid AskOutput JSON: %v\n%s", err, out.String())
	}
	if a := got.Answers["q_noul"]; a.Status != StatusOK || a.Noul == nil || *a.Noul != 0.8 {
		t.Errorf("got %+v", got)
	}
}

// TestCLI_ExitCode_InvalidResponse covers the only non-zero exit branch
// in ask's mapping: a requested question whose answer is missing/
// malformed.
func TestCLI_ExitCode_InvalidResponse(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"good": json.RawMessage(`{"type":"noul","noul":0.5}`),
		// "bad" missing entirely
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--state", `"s"`,
		"--questions", `{"good":{"type":"noul","instructions":"x","criteria":{"true":"t","false":"f"}},"bad":{"type":"noul","instructions":"y","criteria":{"true":"t","false":"f"}}}`,
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
		t.Errorf("exit code = %d, want 2 (invalid_response)", *codePtr)
	}
}

// TestCLI_StateFlagRequiresJSON documents --state's exact behavior for
// jev_ask: AskInput.State is typed `any` (json kind Interface), which
// cliinput.Bind treats as a structured field (see isStructuredKind), so
// its flag value must already be valid JSON -- a bare, unquoted word is
// REJECTED, not treated as a literal string.
func TestCLI_StateFlagRequiresJSON(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{
		"--state", "hello", // NOT valid JSON (bare word, unquoted)
		"--questions", `{"q":{"type":"noul","instructions":"i","criteria":{"true":"t","false":"f"}}}`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --state hello (unquoted, not valid JSON) to be rejected")
	}
	if !strings.Contains(err.Error(), "--state") {
		t.Errorf("error should name the offending flag: %v", err)
	}
}

// TestCLI_StateFlagAcceptsJSONString is the positive counterpart to
// TestCLI_StateFlagRequiresJSON: a JSON-quoted string is accepted and
// decodes to the plain Go string a caller would expect.
func TestCLI_StateFlagAcceptsJSONString(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"q": json.RawMessage(`{"type":"noul","noul":0.5}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--state", `"hello"`, // JSON-quoted string -- accepted
		"--questions", `{"q":{"type":"noul","instructions":"i","criteria":{"true":"t","false":"f"}}}`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--state", `"s"`}) // missing --questions
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing questions")
	}
}
