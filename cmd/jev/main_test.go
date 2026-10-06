package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
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
		"jev_ask", "jev_check", "jev_classify", "jev_compare", "jev_decide",
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
		"ask", "check", "classify", "compare", "decide",
		"doctor", "extract", "gate", "match", "mcp", "models", "rerank",
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
