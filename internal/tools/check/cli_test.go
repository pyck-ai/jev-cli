package check

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
// SystemOne server, mirroring newTestHandler's setup in check_test.go
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
		"p0": json.RawMessage(`{"type":"noul","noul":0.92}`),
	}, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--propositions", `["the sky is blue"]`})
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
		"p0": json.RawMessage(`{"type":"noul","noul":0.92}`),
	}, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"propositions":["x"]}`})
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
	cmd.SetArgs([]string{"--json", `{"propositions":["x"]}`, "--propositions", `["y"]`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--propositions") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"p0": json.RawMessage(`{"type":"noul","noul":0.92}`),
	}, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--propositions", `["x"]`, "-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got CheckOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid CheckOutput JSON: %v\n%s", err, out.String())
	}
	if len(got.Results) != 1 || got.Results[0].Label != "likely" || got.Results[0].Action != ActionAuto {
		t.Errorf("got %+v", got)
	}
	if got.Usage == nil || got.Usage.CostUSD == nil || *got.Usage.CostUSD != 0.00001 {
		t.Errorf("usage.cost_usd = %+v, want 0.00001", got.Usage)
	}
	var raw struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	if _, ok := raw.Usage["cost_usd"]; !ok {
		t.Errorf("raw JSON usage block has no cost_usd: %v", raw.Usage)
	}
}

func TestCLI_ExitCodeReview(t *testing.T) {
	// noul 0.5 is "uncertain" (neither >= default auto_accept 0.85 nor
	// <= 1-0.85) -> Action review -> exit 1.
	srv := fakeServer(t, map[string]json.RawMessage{
		"p0": json.RawMessage(`{"type":"noul","noul":0.5}`),
	}, &openrouter.Usage{Cost: 0.00001})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--propositions", `["it might rain"]`})
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
