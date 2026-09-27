package extract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// cliTestProvider builds a registry.DepsProvider pointed at a fake
// SystemOne server, mirroring newTestHandler's setup in extract_test.go
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

func TestCLI_FlagsBasedInvocation_AutoAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"123","confidence":0.9,"probabilities":{"123":0.9,"none_of_them":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	// Fields is a structured (slice-of-struct) field, so its per-field
	// flag value must be JSON-encoded -- see cliinput's package doc
	// comment.
	cmd.SetArgs([]string{
		"--document", "the code is 123",
		"--fields", `[{"id":"num","pattern":"[0-9]+","description":"the code"}]`,
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
	if !strings.Contains(out.String(), "fields:") || !strings.Contains(out.String(), "123") {
		t.Errorf("text output missing expected fields:\n%s", out.String())
	}
}

// TestCLI_ZeroMatchesNoAPICall is the CLI-level counterpart to
// extract_test.go's TestExtractHandler_Handle_ZeroMatchesMakesNoAPICallAtAll:
// a field whose regex matches nothing must short-circuit before any
// network call, and the overall run must still exit 0 (not_found is not
// an error -- see exitCode's doc comment).
func TestCLI_ZeroMatchesNoAPICall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--document", "nothing interesting here",
		"--fields", `[{"id":"email","pattern":"[a-z]+@[a-z]+","description":"an email address"}]`,
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
	if called {
		t.Fatal("expected NO API call at all when every field has zero regex matches")
	}
	if *codePtr != 0 {
		t.Errorf("exit code = %d, want 0", *codePtr)
	}
}

func TestCLI_JSONInputLiteral(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"7","confidence":0.9,"probabilities":{"7":0.9,"none_of_them":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--json", `{"document":"lucky 7","fields":[{"id":"num","pattern":"[0-9]+","description":"the number"}]}`})
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
	cmd.SetArgs([]string{"--json", `{"document":"x"}`, "--document", "y"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "--document") {
		t.Errorf("error should name conflicting flag: %v", err)
	}
}

func TestCLI_OutputJSON_MatchesOutputStruct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"123","confidence":0.9,"probabilities":{"123":0.9,"none_of_them":0.1}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--document", "the code is 123",
		"--fields", `[{"id":"num","pattern":"[0-9]+","description":"the code"}]`,
		"-o", "json",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got ExtractOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid ExtractOutput JSON: %v\n%s", err, out.String())
	}
	if len(got.Fields) != 1 || got.Fields[0].Status != StatusAuto {
		t.Errorf("got %+v", got)
	}
}

// TestCLI_ExitCode_InvalidPattern covers the exit-2 branch via a field
// whose regex fails to compile -- no server needed, since an invalid
// pattern is caught before any candidate is found (and thus before any
// model call would be needed).
func TestCLI_ExitCode_InvalidPattern(t *testing.T) {
	cmd := newCLICommand(cliTestProvider(t, "http://unused.invalid"), "desc")
	cmd.SetArgs([]string{
		"--document", "hello",
		"--fields", `[{"id":"bad","pattern":"(unclosed","description":"a broken pattern"}]`,
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
		t.Errorf("exit code = %d, want 2 (invalid_pattern)", *codePtr)
	}
}

// TestCLI_ExitCode_Review covers the exit-1 branch: the model confidently
// picks "none of them", which the tool reports as status=review (not
// fabricated) rather than a value.
func TestCLI_ExitCode_Review(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"num": json.RawMessage(`{"type":"choice","choice":"` + noneOfThem + `","confidence":0.9,"probabilities":{}}`),
			},
			Model: "m", Usage: &openrouter.Usage{Cost: 0.00001},
		})
	}))
	defer srv.Close()

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{
		"--document", "call 12345 or 67890",
		"--fields", `[{"id":"num","pattern":"[0-9]+","description":"the relevant number"}]`,
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
