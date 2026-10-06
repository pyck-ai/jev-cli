package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/record"
	"github.com/pyck-ai/jev-cli/internal/registry"
	"github.com/pyck-ai/jev-cli/internal/tools/score"
)

// TestEndToEnd_MCPWireProtocol drives the jev_score tool through a real MCP
// client/server session (in-memory transport, but still real JSON-RPC
// framing and JSON marshal/unmarshal on both ends -- see
// mcp.NewInMemoryTransports), against a fake OpenRouter SystemOne server.
//
// This is the one test in this codebase that exercises the go-sdk's own
// input/output JSON Schema inference and validation (mcp.AddTool), which
// the more targeted tests in internal/tools/score bypass by calling
// ScoreHandler.Handle directly. In particular it confirms:
//   - ScoreInput's required fields (state, scale_min, scale_max,
//     instructions) are correctly inferred as required from the struct's
//     json tags (no `omitempty`).
//   - ScoreOutput -- including the nullable *Usage pointer field -- passes
//     the SDK's own output-schema validation.
//   - A Go error returned from the handler surfaces as IsError=true on the
//     wire, not as a fabricated structured result.
//
// It also, incidentally, exercises the real registry/blank-import plugin
// wiring end to end (newServer below loops over registry.All() exactly as
// main() does): every tool package main.go blank-imports is registered
// against the server this test drives, not just jev_score, though only
// jev_score is actually called here.
func TestEndToEnd_MCPWireProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"score": json.RawMessage(`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`),
			},
			Model: "typesafe/jev-1.13-20260917",
			Usage: &openrouter.Usage{Cost: 0.000019992, InputTokens: 476, OutputTokens: 70},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	client := openrouter.NewClientWithEndpoint("test-key", srv.URL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	auditLog := audit.NewLogger(t.TempDir() + "/audit.jsonl")
	deps := &registry.Deps{
		Client: client,
		Config: cfg,
		Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession),
		Audit:  auditLog,
	}

	server := newServer(deps, registry.All())
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t1, t2 := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()

	clientSession, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	t.Run("valid call round-trips a structured, schema-valid result", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"state":        "some diff to judge",
				"scale_min":    0,
				"scale_max":    2,
				"instructions": "0=incorrect, 1=partially correct, 2=fully correct",
			},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected success, got IsError=true, content=%+v", res.Content)
		}

		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshaling StructuredContent: %v", err)
		}
		var out score.ScoreOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshaling StructuredContent into ScoreOutput: %v\nraw: %s", err, raw)
		}
		if out.Status != score.StatusOK {
			t.Errorf("status = %q, want %q", out.Status, score.StatusOK)
		}
		if out.Score != 1.99 {
			t.Errorf("score = %v, want 1.99", out.Score)
		}
		if out.Usage == nil || out.Usage.InputTokens != 476 {
			t.Errorf("usage = %+v, want input_tokens=476", out.Usage)
		}
	})

	t.Run("missing required field is rejected by input schema validation before the handler runs", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"scale_min":    0,
				"scale_max":    2,
				"instructions": "missing state field entirely",
			},
		})
		// Schema-validation failures surface as a tool error result
		// (IsError=true), not a transport-level Go error, consistent with
		// how the SDK reports other handler-level failures.
		if err != nil {
			t.Fatalf("CallTool transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true for missing required field 'state'")
		}
	})

	t.Run("scale_max <= scale_min surfaces as a tool error, not a fabricated score", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"state":        "x",
				"scale_min":    5,
				"scale_max":    2,
				"instructions": "y",
			},
		})
		if err != nil {
			t.Fatalf("CallTool transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true for scale_max <= scale_min")
		}
		if res.StructuredContent != nil {
			t.Errorf("expected no structured content alongside a tool error, got %#v", res.StructuredContent)
		}
	})
}

// TestNewServer_RegistersEveryToolExactlyOnce is a regression guard for
// the self-registering plugin architecture itself (see internal/registry's
// package doc comment): it lists every tool newServer actually registered
// against a real MCP server/client session and confirms it's exactly the
// expected 14-tool roster (jev_score plus the 13 tools added on top of the
// plugin architecture), with no duplicate names. mcp.Server.AddTool's own
// documented behavior for a name collision is to silently REPLACE the
// earlier tool, not error or panic -- so this test, not just a successful
// build, is what actually catches two tool packages accidentally
// registering the same name.
func TestNewServer_RegistersEveryToolExactlyOnce(t *testing.T) {
	deps := &registry.Deps{
		Client: openrouter.NewClientWithEndpoint("test-key", "http://127.0.0.1:0", openrouter.RetryPolicy{MaxAttempts: 1}),
		Config: config.Default(),
		Budget: budget.NewTracker(0),
		Audit:  audit.NewLogger(t.TempDir() + "/audit.jsonl"),
	}
	server := newServer(deps, registry.All())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t1, t2 := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	clientSession, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := []string{
		"jev_ask", "jev_batch", "jev_check", "jev_classify", "jev_compare", "jev_decide",
		"jev_doctor", "jev_extract", "jev_gate", "jev_match", "jev_rerank",
		"jev_review", "jev_score", "jev_screen", "jev_verify",
	}

	seen := make(map[string]int, len(res.Tools))
	for _, tool := range res.Tools {
		seen[tool.Name]++
	}
	if len(seen) != len(res.Tools) {
		t.Errorf("server reported %d tools but only %d distinct names -- a name collision silently replaced a tool: %v", len(res.Tools), len(seen), seen)
	}
	for _, name := range want {
		if seen[name] != 1 {
			t.Errorf("expected exactly 1 tool named %q, got %d", name, seen[name])
		}
	}
	if len(res.Tools) != len(want) {
		gotNames := make([]string, len(res.Tools))
		for i, tool := range res.Tools {
			gotNames[i] = tool.Name
		}
		t.Errorf("len(Tools) = %d, want %d\ngot:  %v\nwant: %v", len(res.Tools), len(want), gotNames, want)
	}
}

// TestEveryToolExceptDoctorAndBatchHasRun is the tripwire that keeps jev_batch
// complete: a new tool that forgets its `Run:` line would silently be
// unbatchable. doctor is not a judgment and batch cannot nest.
func TestEveryToolExceptDoctorAndBatchHasRun(t *testing.T) {
	for _, tl := range registry.All() {
		excluded := tl.Name == "doctor" || tl.Name == "batch"
		if excluded && tl.Run != nil {
			t.Errorf("tool %q must not be batchable but has Run set", tl.Name)
		}
		if !excluded && tl.Run == nil {
			t.Errorf("tool %q has no Run: add `Run: registry.Runner(...)` to its Register call", tl.Name)
		}
	}
}

// noDepsProvider fails the test if a tool actually tries to build its
// dependencies: none of the tests below run a tool.
func noDepsProvider(t *testing.T) func(string) *registry.Deps {
	return func(string) *registry.Deps {
		t.Fatal("deps provider must not be invoked when building the command tree, printing help, or running mcp")
		return nil
	}
}

// TestNewRootCmd_Subcommands is the CLI-side counterpart to
// TestNewServer_RegistersEveryToolExactlyOnce: the root must have exactly
// one subcommand per tool plus `mcp`. It's a tripwire: update want when a
// tool is added or removed.
func TestNewRootCmd_Subcommands(t *testing.T) {
	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})

	want := []string{
		"ask", "batch", "check", "classify", "compare", "decide",
		"doctor", "extract", "gate", "match", "mcp", "models", "record", "rerank",
		"review", "score", "screen", "verify",
	}
	var got []string
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue // cobra's own built-ins, not a jev tool
		}
		got = append(got, c.Name())
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("subcommands = %v, want %v", got, want)
	}
}

// TestRootCmd_NoArgsIsNotImplementedTUI: plain `jev` is reserved for the
// interactive TUI, which doesn't exist yet. It must fail (main turns that
// into exit 3) instead of silently starting the MCP server or printing help.
func TestRootCmd_NoArgsIsNotImplementedTUI(t *testing.T) {
	mcpStarted := false
	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) { mcpStarted = true })
	root.SetArgs([]string{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)

	err := root.Execute()
	if !errors.Is(err, errTUINotImplemented) {
		t.Fatalf("Execute() error = %v, want errTUINotImplemented", err)
	}
	if mcpStarted {
		t.Error("plain `jev` started the MCP server; that is now `jev mcp`")
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Errorf("not-implemented error should not dump usage; got:\n%s", out.String())
	}
}

func TestRootCmd_McpStartsServer(t *testing.T) {
	mcpStarted := false
	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) { mcpStarted = true })
	root.SetArgs([]string{"mcp"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(mcp) error = %v", err)
	}
	if !mcpStarted {
		t.Error("`jev mcp` did not start the MCP server")
	}
}

func TestRootCmd_HelpDescribesModes(t *testing.T) {
	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	root.SetArgs([]string{"--help"})
	var out bytes.Buffer
	root.SetOut(&out)

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(--help) error = %v", err)
	}
	help := out.String()
	for _, want := range []string{"NOT IMPLEMENTED YET", "jev mcp", "Tool commands:", "Server:", "score", "mcp"} {
		if !strings.Contains(help, want) {
			t.Errorf("--help output missing %q:\n%s", want, help)
		}
	}
}

func TestSelectTools(t *testing.T) {
	all := registry.All()

	got, err := selectTools(nil)
	if err != nil || len(got) != len(all) {
		t.Fatalf("selectTools(nil) = %d tools, err %v; want all %d", len(got), err, len(all))
	}

	// Registration order is kept, jev_ prefixes and duplicates are accepted.
	got, err = selectTools([]string{"verify", "jev_check", " compare ", "check"})
	if err != nil {
		t.Fatalf("selectTools: %v", err)
	}
	names := toolCLINames(got)
	var want []string
	for _, n := range toolCLINames(all) {
		if n == "verify" || n == "check" || n == "compare" {
			want = append(want, n)
		}
	}
	if !slices.Equal(names, want) {
		t.Errorf("selected %v, want %v", names, want)
	}

	_, err = selectTools([]string{"verfiy"})
	if err == nil || !strings.Contains(err.Error(), `unknown tool "verfiy"`) || !strings.Contains(err.Error(), "verify") {
		t.Errorf("unknown tool error = %v, want it to name the bad tool and list valid ones", err)
	}
}

func TestRootCmd_McpToolsFlag(t *testing.T) {
	var served []registry.Tool
	root := newRootCmd(noDepsProvider(t), func(tools []registry.Tool, _ string) { served = tools })
	root.SetArgs([]string{"mcp", "--tools", "verify,check"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := toolCLINames(served); len(got) != 2 || !slices.Contains(got, "verify") || !slices.Contains(got, "check") {
		t.Errorf("served %v, want [verify check] in registration order", got)
	}

	served = nil
	root = newRootCmd(noDepsProvider(t), func(tools []registry.Tool, _ string) { served = tools })
	root.SetArgs([]string{"mcp", "--tools", "nope"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	if err := root.Execute(); err == nil {
		t.Error("expected an error for an unknown --tools name")
	}
	if served != nil {
		t.Error("server must not start when --tools names an unknown tool")
	}
}

// TestNewServer_OnlySelectedTools: tools left out by --tools must not
// appear in the MCP tool list at all, since that is what keeps them out of
// the model's context.
func TestNewServer_OnlySelectedTools(t *testing.T) {
	deps := &registry.Deps{
		Client: openrouter.NewClientWithEndpoint("test-key", "http://127.0.0.1:0", openrouter.RetryPolicy{MaxAttempts: 1}),
		Config: config.Default(),
		Budget: budget.NewTracker(0),
		Audit:  audit.NewLogger(t.TempDir() + "/audit.jsonl"),
	}
	tools, err := selectTools([]string{"verify", "check"})
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(deps, tools)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t1, t2 := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "v0"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"jev_check", "jev_verify"}) {
		t.Errorf("tools/list = %v, want [jev_check jev_verify]", got)
	}
}

// TestRootCmd_ModelFlagIsPersistentAndReachesDeps: --model is registered on
// the root as a persistent flag, so every tool subcommand and `mcp` accept
// it; its value reaches the lazy deps builder (where buildDeps forces it
// onto the loaded config) and the MCP server entry point, and is "" when
// unset.
func TestRootCmd_ModelFlagIsPersistentAndReachesDeps(t *testing.T) {
	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	if f := root.PersistentFlags().Lookup("model"); f == nil {
		t.Fatal("root has no persistent --model flag")
	}
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if c.InheritedFlags().Lookup("model") == nil {
			t.Errorf("subcommand %q does not inherit --model", c.Name())
		}
	}

	t.Run("mcp receives the flag value", func(t *testing.T) {
		var got = "unset"
		root := newRootCmd(noDepsProvider(t), func(_ []registry.Tool, model string) { got = model })
		root.SetArgs([]string{"mcp", "--model", "liquid/d1"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got != "liquid/d1" {
			t.Errorf("serveMCP model = %q, want %q", got, "liquid/d1")
		}
	})

	t.Run("tool subcommand: deps builder gets the value and the config is forced", func(t *testing.T) {
		type stop struct{}
		var got = "unset"
		var cfg config.Config
		root := newRootCmd(func(model string) *registry.Deps {
			got = model
			cfg = config.Default()
			cfg.ToolModelOverrides = map[string]string{"jev_score": "typesafe/jev-1.13"}
			cfg.ForceModel(model)
			panic(stop{}) // deps are built lazily, at tool run time; end the run here
		}, func([]registry.Tool, string) {})
		root.SetArgs([]string{"score", "--model", "cloudflare/clef", "--state", "x", "--scale-min", "0", "--scale-max", "2", "--instructions", "i"})
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(stop); !ok {
						panic(r)
					}
				}
			}()
			_ = root.Execute()
		}()
		if got != "cloudflare/clef" {
			t.Fatalf("deps builder model = %q, want %q (score may have failed before building deps)", got, "cloudflare/clef")
		}
		if m := cfg.ModelForTool("jev_score"); m != "cloudflare/clef" {
			t.Errorf("ModelForTool(jev_score) = %q, want the --model value to beat tool_model_overrides", m)
		}
	})

	t.Run("unset flag is empty", func(t *testing.T) {
		var got = "unset"
		root := newRootCmd(noDepsProvider(t), func(_ []registry.Tool, model string) { got = model })
		root.SetArgs([]string{"mcp"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got != "" {
			t.Errorf("serveMCP model = %q, want empty", got)
		}
	})
}

// TestRootCmd_ModelsCommand: `jev models` is registered in the Info group,
// listed in --help without building deps, and, when run, reads the model
// list through the deps' client (here a fake API; cache in a temp dir).
func TestRootCmd_ModelsCommand(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	root.SetArgs([]string{"--help"})
	var help bytes.Buffer
	root.SetOut(&help)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "Info:") || !strings.Contains(help.String(), "models") {
		t.Errorf("--help lacks the Info group / models:\n%s", help.String())
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Write([]byte(`{"data":[{"id":"liquid/d1","context_length":65536}]}`))
	}))
	defer srv.Close()
	client := openrouter.NewClientWithRoutes(openrouter.Route{Name: openrouter.RouteDirect, BaseURL: srv.URL + "/api/v1", APIKey: "k"}, nil, openrouter.RetryPolicy{MaxAttempts: 1})

	var gotModel string
	root = newRootCmd(func(model string) *registry.Deps {
		gotModel = model
		return &registry.Deps{Client: client}
	}, func([]registry.Tool, string) {})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--model", "liquid/d1", "models"})
	if err := root.Execute(); err != nil {
		t.Fatalf("jev models: %v", err)
	}
	if gotPath != "/api/v1/models?output_modalities=decisions" {
		t.Errorf("requested %q", gotPath)
	}
	if gotModel != "liquid/d1" {
		t.Errorf("deps built with model %q, want the --model value", gotModel)
	}
	if !strings.Contains(out.String(), "liquid/d1") || !strings.Contains(out.String(), "65536") {
		t.Errorf("output = %q", out.String())
	}
}

// abortHook is a stand-in for the preflight guard that always vetoes.
type abortHook struct{}

func (abortHook) BeforeAsk(context.Context, *openrouter.Request) error {
	return errors.New("preflight: too big")
}
func (abortHook) AfterAsk(context.Context, *openrouter.Request, int, []byte, *openrouter.Response, error, time.Duration) {
}

func readRecords(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("want exactly 1 recording file in %s, got %v", dir, files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestEndToEnd_Recording drives jev_score over MCP with recording on and
// checks the session, client, tool_call and systemone records, their
// call_id correlation, that a guard veto is recorded, that no key leaks,
// and file permissions.
func TestEndToEnd_Recording(t *testing.T) {
	const secret = "sk-or-FAKE-SECRET-KEY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers:  map[string]json.RawMessage{"score": json.RawMessage(`{"type":"score","score":1.5,"confidence":0.9,"probabilities":{"0":0,"1":0.5,"2":0.5}}`)},
			Model:    "typesafe/jev-1.13-20260917",
			Provider: "TypeSafe",
			Usage:    &openrouter.Usage{Cost: 0.00002, InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer srv.Close()

	run := func(t *testing.T, dir string, guard openrouter.Hook) {
		cfg := config.Default()
		client := openrouter.NewClientWithEndpoint(secret, srv.URL, openrouter.RetryPolicy{MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 5})
		rec := record.New(dir, time.Now(), 4242)
		installHooks(client, rec, guard)
		deps := &registry.Deps{Client: client, Config: cfg, Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession), Audit: audit.NewLogger(t.TempDir() + "/audit.jsonl")}
		server := newRecordedServer(deps, registry.All(), rec)
		rec.Session(sessionRecord("mcp", deps, registry.All(), nil, ""))
		defer rec.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		t1, t2 := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ss.Close()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v9"}, nil).Connect(ctx, t2, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: score.ToolNameScore, Arguments: map[string]any{
			"state": "secret judged content", "scale_min": 0, "scale_max": 2, "instructions": "rate",
		}}); err != nil {
			t.Fatal(err)
		}
	}

	check := func(t *testing.T, dir string, wantPreflight bool) {
		recs := readRecords(t, dir)
		by := map[string][]map[string]any{}
		for _, r := range recs {
			by[r["kind"].(string)] = append(by[r["kind"].(string)], r)
			if r["session"] != "" && r["session"] == nil {
				t.Errorf("missing session: %v", r)
			}
		}
		if len(by["session"]) != 1 || len(by["tool_call"]) != 1 || len(by["systemone"]) != 1 || len(by["client"]) != 1 {
			t.Fatalf("record kinds = %v", by)
		}
		tc, so := by["tool_call"][0], by["systemone"][0]
		if tc["tool"] != score.ToolNameScore || tc["call_id"] == "" || tc["call_id"] != so["call_id"] {
			t.Errorf("tool_call/systemone call_id mismatch: %v vs %v", tc, so)
		}
		if !strings.Contains(string(mustJSON(tc["arguments"])), "secret judged content") {
			t.Errorf("tool_call lacks full arguments: %v", tc)
		}
		if tc["client"].(map[string]any)["name"] != "test-client" {
			t.Errorf("client not recorded: %v", tc)
		}
		if !strings.Contains(string(mustJSON(so["request"])), "secret judged content") {
			t.Errorf("systemone lacks full request: %v", so)
		}
		if wantPreflight {
			if so["preflight_error"] != "preflight: too big" || so["http_status"] != nil {
				t.Errorf("preflight abort not recorded: %v", so)
			}
		} else if so["http_status"] != float64(200) || so["provider"] != "TypeSafe" || so["response"] == nil || so["usage"] == nil {
			t.Errorf("systemone wire fields missing: %v", so)
		}

		files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		raw, _ := os.ReadFile(files[0])
		if strings.Contains(string(raw), secret) || strings.Contains(strings.ToLower(string(raw)), "authorization") {
			t.Error("recording leaked the API key or an Authorization header")
		}
		if fi, _ := os.Stat(files[0]); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
		}
	}

	t.Run("wire call", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rec")
		run(t, dir, nil)
		check(t, dir, false)
	})
	t.Run("preflight abort is recorded", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "rec")
		run(t, dir, abortHook{})
		check(t, dir, true)
	})
	t.Run("off creates nothing", func(t *testing.T) {
		parent := t.TempDir()
		run(t, "", nil)
		if entries, _ := os.ReadDir(parent); len(entries) != 0 {
			t.Errorf("files created: %v", entries)
		}
	})
}

// TestEndToEnd_Batch drives jev_batch over the in-memory MCP transport with
// recording on: tools/list includes it with a plain-object item input, a call
// with 2 decide + 1 check items returns structured per-item results in input
// order, an invalid item fails alone, a malformed batch is a tool error, and
// every systemone record shares the one tool_call's call_id.
func TestEndToEnd_Batch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]json.RawMessage{}
		for k := range req.Questions {
			if k == "decision" {
				answers[k] = json.RawMessage(`{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}`)
			} else {
				answers[k] = json.RawMessage(`{"type":"noul","noul":0.9}`)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openrouter.Response{Answers: answers, Model: "m", Usage: &openrouter.Usage{Cost: 0.00002}})
	}))
	defer srv.Close()

	cfg := config.Default()
	client := openrouter.NewClientWithEndpoint("test-key", srv.URL, openrouter.RetryPolicy{MaxAttempts: 1, BaseBackoffMs: 1, MaxBackoffMs: 5})
	dir := filepath.Join(t.TempDir(), "rec")
	rec := record.New(dir, time.Now(), 4242)
	installHooks(client, rec, nil)
	deps := &registry.Deps{Client: client, Config: cfg, Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession), Audit: audit.NewLogger(t.TempDir() + "/audit.jsonl")}
	server := newRecordedServer(deps, registry.All(), rec)
	rec.Session(sessionRecord("mcp", deps, registry.All(), nil, ""))
	defer rec.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t1, t2 := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v9"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *mcp.Tool
	for _, tool := range lt.Tools {
		if tool.Name == "jev_batch" {
			found = tool
		}
	}
	if found == nil {
		t.Fatal("tools/list lacks jev_batch")
	}
	if schema := string(mustJSON(found.InputSchema)); !strings.Contains(schema, `"input":{"additionalProperties":true`) || !strings.Contains(schema, `"type":"object"`) {
		t.Errorf("jev_batch input schema does not describe item input as a plain object: %s", schema)
	}

	decideIn := func(pick string) map[string]any {
		return map[string]any{"decision": "d " + pick, "evidence": "e", "priorities": "p", "candidates": []any{
			map[string]any{"id": "a", "description": "A"}, map[string]any{"id": "b", "description": "B"}}}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jev_batch", Arguments: map[string]any{"items": []any{
		map[string]any{"id": "q1", "tool": "decide", "input": decideIn("1")},
		map[string]any{"id": "bad", "tool": "decide", "input": map[string]any{"decision": "no candidates"}},
		map[string]any{"id": "q2", "tool": "jev_decide", "input": decideIn("2")},
		map[string]any{"id": "sanity", "tool": "check", "input": map[string]any{"context": "c", "propositions": []any{"p"}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("IsError: %+v", res.Content)
	}
	var out struct {
		Results []struct {
			Index    int             `json:"index"`
			ID       string          `json:"id"`
			Tool     string          `json:"tool"`
			Status   string          `json:"status"`
			ExitCode int             `json:"exit_code"`
			Output   json.RawMessage `json:"output"`
			Error    string          `json:"error"`
		} `json:"results"`
		Summary struct {
			Items, OK, Error int
			ExitCode         int `json:"exit_code"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(mustJSON(res.StructuredContent), &out); err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"q1", "bad", "q2", "sanity"}
	wantStatus := []string{"ok", "error", "ok", "ok"}
	if len(out.Results) != 4 {
		t.Fatalf("results = %+v", out.Results)
	}
	for i, r := range out.Results {
		if r.Index != i || r.ID != wantIDs[i] || r.Status != wantStatus[i] {
			t.Errorf("results[%d] = %+v", i, r)
		}
	}
	if out.Results[1].Error == "" || len(out.Results[1].Output) != 0 || out.Results[0].Output == nil {
		t.Errorf("error/output fields wrong: %+v", out.Results[:2])
	}
	if out.Summary.Items != 4 || out.Summary.OK != 3 || out.Summary.Error != 1 || out.Summary.ExitCode != 3 {
		t.Errorf("summary = %+v", out.Summary)
	}

	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "jev_batch", Arguments: map[string]any{"items": []any{
		map[string]any{"tool": "doctor", "input": map[string]any{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bad.IsError {
		t.Error("batch containing doctor must be a tool error")
	}

	// Recording: all systemone records of the first batch call share its call_id.
	var batchCall string
	var soIDs []string
	for _, r := range readRecords(t, dir) {
		switch r["kind"] {
		case "tool_call":
			if r["tool"] == "jev_batch" && batchCall == "" {
				batchCall = r["call_id"].(string)
			}
		case "systemone":
			soIDs = append(soIDs, r["call_id"].(string))
		}
	}
	if batchCall == "" || len(soIDs) != 3 {
		t.Fatalf("batchCall=%q systemone call_ids=%v, want 3 records", batchCall, soIDs)
	}
	for _, id := range soIDs {
		if id != batchCall {
			t.Errorf("systemone call_id %q != jev_batch tool_call %q", id, batchCall)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// TestRootCmd_RecordFlagAndEnv checks flag-over-env precedence, that help
// creates nothing, and that the flag is persistent.
func TestRootCmd_RecordFlagAndEnv(t *testing.T) {
	t.Cleanup(func() { activeRecorder = nil })
	envDir, flagDir := t.TempDir(), t.TempDir()
	t.Setenv(record.EnvDir, envDir)

	root := newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	root.SetArgs([]string{"mcp"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if activeRecorder == nil || filepath.Dir(activeRecorder.Path()) != envDir {
		t.Errorf("env not honored: %v", activeRecorder.Path())
	}

	root = newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	root.SetArgs([]string{"--record", flagDir, "mcp"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(activeRecorder.Path()) != flagDir {
		t.Errorf("flag should win: %v", activeRecorder.Path())
	}

	t.Setenv(record.EnvDir, "")
	root = newRootCmd(noDepsProvider(t), func([]registry.Tool, string) {})
	root.SetArgs([]string{"mcp"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if activeRecorder != nil {
		t.Error("recording should be off with no flag and no env")
	}
	for _, d := range []string{envDir, flagDir} {
		if e, _ := os.ReadDir(d); len(e) != 0 {
			t.Errorf("files created in %s: %v", d, e)
		}
	}
}
