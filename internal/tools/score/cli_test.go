package score

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
// SystemOne server, mirroring newTestHandler's setup in score_test.go
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
// a pointer to the code it was last called with (or nil if never
// called).
func withRecordedExit(t *testing.T) *int {
	t.Helper()
	var code *int
	old := exitFunc
	exitFunc = func(c int) {
		got := c
		code = &got
	}
	t.Cleanup(func() { exitFunc = old })
	return code
}

func TestCLI_FlagsBasedInvocation(t *testing.T) {
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`,
		&openrouter.Usage{Cost: 0.00001, InputTokens: 10, OutputTokens: 5})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--state", "hello", "--scale-min", "0", "--scale-max", "2", "--instructions", "rate it"})
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
	if !strings.Contains(out.String(), "score:") || !strings.Contains(out.String(), "status:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":0.5,"confidence":0.7,"probabilities":{"0":0.5,"1":0.5}}`,
		&openrouter.Usage{Cost: 0.00001, InputTokens: 10, OutputTokens: 5})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"state":"x","scale_min":0,"scale_max":1,"instructions":"y"}`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := new(int)
	exitFunc = func(c int) { *codePtr = c }

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
}

func TestCLI_JSONFlagConflictsWithFieldFlag(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--json", `{"state":"x"}`, "--state", "y"})
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
	srv := fakeSystemOneServer(t,
		`{"type":"score","score":1.0,"confidence":0.9,"probabilities":{"0":0.1,"1":0.9}}`,
		&openrouter.Usage{Cost: 0.00001, InputTokens: 10, OutputTokens: 5})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--state", "x", "--scale-min", "0", "--scale-max", "1", "--instructions", "y", "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got ScoreOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid ScoreOutput JSON: %v\n%s", err, out.String())
	}
	if got.Status != StatusOK || got.Score != 1.0 {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeInvalidResponse(t *testing.T) {
	// A malformed answer (missing "score" type discriminator) yields
	// status=invalid_response, which maps to exit 3.
	srv := fakeSystemOneServer(t, `{"type":"noul","noul":0.5}`, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--state", "x", "--scale-min", "0", "--scale-max", "1", "--instructions", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := new(int)
	exitFunc = func(c int) { *codePtr = c }

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 3 {
		t.Errorf("exit code = %d, want 3", *codePtr)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--scale-min", "0", "--scale-max", "1", "--instructions", "y"}) // missing --state
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing state")
	}
}
