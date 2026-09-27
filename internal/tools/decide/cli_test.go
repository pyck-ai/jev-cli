package decide

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// cliTestProvider builds a registry.DepsProvider pointed at a fake
// SystemOne server, mirroring newTestHandler's setup in decide_test.go
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

// decideServer builds a fake SystemOne server returning the given
// answers for any request, mirroring the inline httptest servers
// decide_test.go's own tests build (this package has no shared
// "fakeServer" helper to reuse).
func decideServer(t *testing.T, answers map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: answers,
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.00003},
		})
	}))
}

func baseArgs() []string {
	return []string{
		"--decision", "which vendor to pick",
		"--evidence", "vendor A is cheaper, vendor B is faster",
		"--priorities", "cost matters most",
		"--candidates", `[{"id":"a","description":"Vendor A"},{"id":"b","description":"Vendor B"}]`,
	}
}

func TestCLI_FlagsBasedInvocation(t *testing.T) {
	srv := decideServer(t, map[string]json.RawMessage{
		"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs(baseArgs())
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 0 {
		t.Errorf("exit code = %d, want 0", *code)
	}
	if !strings.Contains(out.String(), "recommendation:") || !strings.Contains(out.String(), "selected:") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := decideServer(t, map[string]json.RawMessage{
		"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"decision":"d","evidence":"e","priorities":"p","candidates":[{"id":"a","description":"A"},{"id":"b","description":"B"}]}`})
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
	cmd.SetArgs([]string{"--json", `{"decision":"d"}`, "--decision", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--decision") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := decideServer(t, map[string]json.RawMessage{
		"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	args := append(baseArgs(), "-o", "json")
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got DecideOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid DecideOutput JSON: %v\n%s", err, out.String())
	}
	if got.Recommendation.Selected != "a" || got.Recommendation.Escaped {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ExitCodeEscapeHatchSelected(t *testing.T) {
	srv := decideServer(t, map[string]json.RawMessage{
		"decision": json.RawMessage(`{"type":"choice","choice":"ask_user","confidence":0.8,"probabilities":{"a":0.2,"b":0.2,"ask_user":0.5,"investigate":0.05,"none":0.05}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs(baseArgs())
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 1 {
		t.Errorf("exit code = %d, want 1 (escape hatch selected)", *code)
	}
}

func TestCLI_ExitCodeInvalidCheck(t *testing.T) {
	// The main "decision" answer is fine, but req0_a is missing entirely
	// -- Checks[i].Answer == AnswerInvalidResponse for that pair, which
	// this package's exitCode folds into the review tier (1). See cli.go's
	// exitCode doc comment for why this goes beyond the literal one-line
	// spec.
	srv := decideServer(t, map[string]json.RawMessage{
		"decision": json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`),
		"req0_b":   json.RawMessage(`{"type":"choice","choice":"unclear","confidence":0.5,"probabilities":{"supported":0.3,"contradicted":0.2,"unclear":0.5}}`),
	})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	args := append(baseArgs(), "--requirements", `["must support SSO"]`)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 1 {
		t.Errorf("exit code = %d, want 1 (a requirement check came back invalid_response)", *code)
	}
}

func TestCLI_ExitCodeRecommendationInvalidResponse(t *testing.T) {
	// The main "decision" answer is missing entirely --
	// Recommendation.Status == invalid_response, which this package's
	// exitCode treats as a hard failure (3), matching the score/rerank
	// precedent for a Status-only verdict field (see cli.go's exitCode
	// doc comment).
	srv := decideServer(t, map[string]json.RawMessage{})
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs(baseArgs())
	var out bytes.Buffer
	cmd.SetOut(&out)
	code := withRecordedExit(t)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *code != 3 {
		t.Errorf("exit code = %d, want 3 (recommendation invalid_response)", *code)
	}
}

func TestCLI_ValidationErrorIsHardFailure(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{"--evidence", "e", "--priorities", "p"}) // missing --decision, --candidates
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected validation error for missing decision/candidates")
	}
}
