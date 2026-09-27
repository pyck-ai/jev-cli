package classify

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
// SystemOne server, mirroring newTestHandler's setup in
// classify_test.go but exposed as the lazy provider shape the CLI
// adapter expects.
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
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.95,"probabilities":{"bug":0.95,"feature":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--items", `[{"id":"i1","text":"it crashes on startup"}]`,
		"--classes", `[{"id":"bug","description":"a defect report"},{"id":"feature","description":"a feature request"}]`,
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 0 {
		t.Errorf("exit code = %d, want 0", *code)
	}
	if !strings.Contains(out.String(), "results:") || !strings.Contains(out.String(), "classification") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.95,"probabilities":{"bug":0.95,"feature":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"items":[{"id":"i1","text":"x"}],"classes":[{"id":"bug","description":"d"},{"id":"feature","description":"d2"}]}`})
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
	cmd.SetArgs([]string{"--json", `{"items":[]}`, "--items", `[{"id":"i1","text":"x"}]`})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--items") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.95,"probabilities":{"bug":0.95,"feature":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--items", `[{"id":"i1","text":"x"}]`,
		"--classes", `[{"id":"bug","description":"d"},{"id":"feature","description":"d2"}]`,
		"-o", "json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got ClassifyOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid ClassifyOutput JSON: %v\n%s", err, out.String())
	}
	if len(got.Results) != 1 || got.Results[0].Classification != "bug" {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeReviewOnMalformedAnswer(t *testing.T) {
	// A malformed answer (choice names an option outside validOptions)
	// yields Status=invalid_response and Decision forced to "review" for
	// that item, which maps to exit 1.
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"not_a_class","confidence":0.9}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--items", `[{"id":"i1","text":"x"}]`,
		"--classes", `[{"id":"bug","description":"d"},{"id":"feature","description":"d2"}]`,
	})
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

func TestCLI_ExitCodeReviewOnLowMargin(t *testing.T) {
	// A well-formed answer whose margin falls below the default
	// minimum_margin (0.5) is Decision=review too, without any
	// Status=invalid_response involved -- exercising the other path into
	// exitCode's review branch.
	srv := fakeServer(t, map[string]json.RawMessage{
		"item0": json.RawMessage(`{"type":"choice","choice":"bug","confidence":0.9,"probabilities":{"bug":0.55,"feature":0.45}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--items", `[{"id":"i1","text":"x"}]`,
		"--classes", `[{"id":"bug","description":"d"},{"id":"feature","description":"d2"}]`,
	})
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

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--classes", `[{"id":"bug","description":"d"}]`}) // missing --items
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing items")
	}
}
