package doctor

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

func TestCLI_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.99}`)},
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.000001, InputTokens: 5, OutputTokens: 2},
		})
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs(nil)
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
	if !strings.Contains(out.String(), "reachable:") || !strings.Contains(out.String(), "true") {
		t.Errorf("output missing reachable:true:\n%s", out.String())
	}
}

func TestCLI_Unreachable_ExitsHard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	cfg := config.Default()
	cfg.Retry.MaxAttempts = 1 // fail fast, no retry delay in the test
	client := openrouter.NewClientWithEndpoint("test-key", srv.URL, openrouter.RetryPolicy{
		MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 1,
	})
	provider := func() *registry.Deps {
		return &registry.Deps{
			Client: client,
			Config: cfg,
			Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession),
			Audit:  audit.NewLogger(t.TempDir() + "/audit.jsonl"),
		}
	}

	cmd := newCLICommand(provider, "desc")
	cmd.SetArgs(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	codePtr := new(int)
	exitFunc = func(c int) { *codePtr = c }

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *codePtr != 3 {
		t.Errorf("exit code = %d, want 3 (unreachable)", *codePtr)
	}
	if !strings.Contains(out.String(), "reachable:") || !strings.Contains(out.String(), "false") {
		t.Errorf("output missing reachable:false:\n%s", out.String())
	}
}

func TestCLI_OutputJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.99}`)},
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.000001, InputTokens: 5, OutputTokens: 2},
		})
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"-o", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got DoctorOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid DoctorOutput JSON: %v\n%s", err, out.String())
	}
	if !got.Reachable {
		t.Errorf("got %+v", got)
	}
}

func TestCLI_ProbeModelFlag(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.5}`)},
			Model:   req.Model,
			Usage:   &openrouter.Usage{Cost: 0.000001},
		})
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	cmd := newCLICommand(cliTestProvider(t, srv.URL), "desc")
	cmd.SetArgs([]string{"--probe-model", "some/other-model"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	exitFunc = func(int) {}

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotModel != "some/other-model" {
		t.Errorf("probe model sent = %q, want %q", gotModel, "some/other-model")
	}
}
