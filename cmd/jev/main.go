// Command jev exposes TypeSafe's "Jev" judgment model, via OpenRouter's
// SystemOne API, as a set of tools: as an MCP (Model Context Protocol)
// server over stdio, and as CLI subcommands (`jev score ...`). See
// README.md for setup, configuration, and the tool reference.
//
// Tools are self-registering plugins (see internal/registry's package doc
// comment for the full mechanism): this file has no knowledge of any
// specific tool beyond the blank imports below, which is what actually
// activates each one. Adding a tool = one new package under
// internal/tools/ plus one new blank-import line here; removing a tool =
// delete that package plus its blank-import line. No other code in this
// file changes either way.
//
// The binary has three modes, all dispatched by one cobra command tree
// (see newRootCmd):
//
//	jev              interactive TUI -- not implemented yet; exits 3 with a
//	                 pointer to the other two modes
//	jev <tool> ...   run one tool non-interactively (jev score, jev verify, ...)
//	jev mcp          serve every tool over MCP on stdio (runMCPServer)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/credentials"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"

	_ "github.com/pyck-ai/jev-cli/internal/tools/ask"      // jev_ask
	_ "github.com/pyck-ai/jev-cli/internal/tools/check"    // jev_check
	_ "github.com/pyck-ai/jev-cli/internal/tools/classify" // jev_classify
	_ "github.com/pyck-ai/jev-cli/internal/tools/compare"  // jev_compare
	_ "github.com/pyck-ai/jev-cli/internal/tools/decide"   // jev_decide
	_ "github.com/pyck-ai/jev-cli/internal/tools/doctor"   // jev_doctor
	_ "github.com/pyck-ai/jev-cli/internal/tools/extract"  // jev_extract
	_ "github.com/pyck-ai/jev-cli/internal/tools/gate"     // jev_gate
	_ "github.com/pyck-ai/jev-cli/internal/tools/match"    // jev_match
	_ "github.com/pyck-ai/jev-cli/internal/tools/rerank"   // jev_rerank
	_ "github.com/pyck-ai/jev-cli/internal/tools/review"   // jev_review
	_ "github.com/pyck-ai/jev-cli/internal/tools/score"    // jev_score
	_ "github.com/pyck-ai/jev-cli/internal/tools/screen"   // jev_screen
	_ "github.com/pyck-ai/jev-cli/internal/tools/verify"   // jev_verify
)

// serverVersion is this MCP server's own version, reported in its
// Implementation metadata (unrelated to the OpenRouter/Jev model version).
const serverVersion = "v0.1.0"

func main() {
	// google/jsonschema-go v0.3.0+ emits "type":["null","array"] for every
	// optional slice field in an auto-inferred schema. At least one MCP
	// client's tool-schema adapter (Google AI Studio / Gemini, observed via
	// opencode) converts that into an "anyOf" and drops "items" on the
	// array branch, producing a "GenerateContentRequest...items: missing
	// field" error -- for EVERY session sharing this MCP server, not just
	// ones calling an affected tool, because all registered tools' schemas
	// ship together in one completions request. This restores the
	// pre-v0.3.0 plain "type":"array" shape (with "items" intact).
	//
	// Set here, at the top of main(), rather than relying on the MCP
	// client (e.g. opencode.json's per-server "environment" block) to
	// inject it into this process's environment before start: an MCP
	// host's "reconnect this server" runtime action commonly respawns the
	// subprocess using server config it already loaded at its own last
	// startup, not a fresh re-read of the client's config file, so a
	// newly-added external env var can go inert until the *host* restarts.
	// Setting it here makes the fix self-contained in this binary --
	// respawning just this subprocess (e.g. via opencode's
	// "/mcp/{name}/connect" API) is sufficient to pick it up. Confirmed
	// live: 2026-09-26, this flag alone is what took a `tools/list` dump
	// from having 12 tools with the null-typed-array pattern to zero.
	if err := os.Setenv("JSONSCHEMAGODEBUG", "typeschemasnull=1"); err != nil {
		fmt.Fprintf(os.Stderr, "jev: warning: could not set JSONSCHEMAGODEBUG: %v\n", err)
	}

	root := newRootCmd(func() *registry.Deps { return buildDeps(3) }, runMCPServer)

	// Exit 3 on any Execute error (unknown command, bad flags, the
	// not-yet-implemented TUI): the CLI exit-code scheme reserves 3 for
	// hard errors, 0/1/2 for verdict outcomes.
	if err := root.Execute(); err != nil {
		os.Exit(3)
	}
}

// runMCPServer is the `jev mcp` mode: build the shared dependencies,
// register the selected tools as MCP tools, and serve MCP over stdio until
// the client disconnects or an error occurs.
func runMCPServer(tools []registry.Tool) {
	deps := buildDeps(1)

	server := newServer(deps, tools)

	// The log line reports the tool count and the fallback default_model
	// (each tool may resolve a different model via
	// cfg.ToolModelOverrides), plus the tool names when --tools limited
	// the set.
	toolsDesc := fmt.Sprint(len(tools))
	if len(tools) < len(registry.All()) {
		toolsDesc += " [" + strings.Join(toolCLINames(tools), ",") + "]"
	}
	fmt.Fprintf(os.Stderr, "jev: starting (tools=%s, default_model=%s, config=%s, audit_log=%s)\n",
		toolsDesc, deps.Config.DefaultModel, mustConfigPath(), mustAuditPath())

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "jev: server exited with error: %v\n", err)
		os.Exit(1)
	}
}

// errTUINotImplemented is returned by the root command when jev runs with
// no arguments, which is reserved for an interactive TUI that doesn't
// exist yet.
var errTUINotImplemented = errors.New("the interactive TUI (jev with no arguments) is not implemented yet; " +
	"run a tool with `jev <command>` (see `jev --help`), or start the MCP server with `jev mcp`")

// Help-output group IDs for the root command's subcommands.
const (
	groupTools  = "tools"
	groupServer = "server"
)

// selectTools resolves `jev mcp --tools` names to registered tools. Names
// are the CLI subcommand names (verify, check, ...); a "jev_" prefix is
// accepted too, so MCP tool names work as well. An empty list selects every
// tool. Duplicates are ignored, and the result keeps registration order.
func selectTools(names []string) ([]registry.Tool, error) {
	all := registry.All()
	if len(names) == 0 {
		return all, nil
	}
	known := make(map[string]bool, len(all))
	for _, t := range all {
		known[t.Name] = true
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		key := strings.TrimPrefix(strings.TrimSpace(n), "jev_")
		if !known[key] {
			return nil, fmt.Errorf("--tools: unknown tool %q; valid tools are: %s (a jev_ prefix is optional)",
				n, strings.Join(toolCLINames(all), ", "))
		}
		want[key] = true
	}
	var selected []registry.Tool
	for _, t := range all {
		if want[t.Name] {
			selected = append(selected, t)
		}
	}
	return selected, nil
}

// toolCLINames returns the CLI subcommand names of tools, in order.
func toolCLINames(tools []registry.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// newRootCmd builds the whole command tree: the root (the TUI placeholder),
// the `mcp` subcommand (which calls serveMCP with the tools selected by
// --tools), and one subcommand per registered tool, added by that tool's
// RegisterCLI hook.
//
// provider is LAZY: it builds the credential/config/audit plumbing (and
// exits the process if that fails) only when a tool subcommand actually
// runs, never while parsing flags or printing help, so `jev --help` and
// `jev score --help` work with no credentials configured. serveMCP is a
// parameter so tests can check the `mcp` wiring without starting a
// server.
func newRootCmd(provider registry.DepsProvider, serveMCP func([]registry.Tool)) *cobra.Command {
	root := &cobra.Command{
		Use:   "jev",
		Short: "TypeSafe's Jev judgment model, as a CLI and an MCP server",
		Long: `jev exposes TypeSafe's Jev judgment model (via OpenRouter's SystemOne API)
as a set of judgment tools, in three modes:

  jev              Interactive TUI. NOT IMPLEMENTED YET: currently prints an
                   error and exits 3.
  jev <command>    Run one tool non-interactively and print its result
                   (text by default, -o json for scripting). The exit code
                   reflects the tool's verdict.
  jev mcp          Serve every tool over MCP on stdio, for MCP clients such
                   as opencode or Claude Code.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A missing feature, not a usage mistake: skip the usage dump.
			cmd.SilenceUsage = true
			return errTUINotImplemented
		},
	}
	root.AddGroup(
		&cobra.Group{ID: groupTools, Title: "Tool commands:"},
		&cobra.Group{ID: groupServer, Title: "Server:"},
	)

	var toolsFlag []string
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server over stdio",
		Long: `Serve jev tools (jev_score, jev_verify, ...) as MCP tools over stdio.
Point your MCP client's server command at ` + "`jev mcp`" + `.

By default every tool is served. Use --tools to serve only some of them,
e.g. to give different agents different tool sets or to keep the tool
list (and its token cost) small:

  jev mcp --tools verify,check,compare`,
		GroupID: groupServer,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			tools, err := selectTools(toolsFlag)
			if err != nil {
				return err
			}
			serveMCP(tools)
			return nil
		},
	}
	mcpCmd.Flags().StringSliceVar(&toolsFlag, "tools", nil,
		"Serve only these tools, comma-separated (e.g. verify,check,compare). Names as in `jev --help`; a jev_ prefix is optional. Default: all tools.")
	root.AddCommand(mcpCmd)

	toolNames := make(map[string]bool)
	for _, t := range registry.All() {
		if t.RegisterCLI != nil {
			t.RegisterCLI(root, provider)
			toolNames[t.Name] = true
		}
	}
	for _, c := range root.Commands() {
		if toolNames[c.Name()] {
			c.GroupID = groupTools
		}
	}
	return root
}

// buildDeps resolves the API key and config and builds the shared
// *registry.Deps every tool's RegisterMCP/RegisterCLI needs -- identically
// for both of jev-cli's run modes. This is exactly the construction
// sequence this file always had before it supported any mode but the MCP
// server, factored out so runCLI can share it rather than duplicating it.
//
// The API key is resolved here (env var, falling back to opencode's own
// stored credentials -- see internal/credentials) and passed around
// out-of-band from *config.Config: per the project's security
// requirements it must never be read from the jev-cli config file,
// logged, or included in any error message, from either source. A key is
// required at startup -- fail fast with a clear error rather than
// deferring the failure to the first tool call.
//
// exitCode is the process exit code used on failure: 1 for the MCP
// server's own startup (historical behavior), 3 for CLI invocations
// (hard-error code in the CLI's exit-code scheme, reserving 0/1/2 for
// verdict outcomes).
func buildDeps(exitCode int) *registry.Deps {
	cred, ok := credentials.Resolve()
	if !ok {
		fmt.Fprintf(os.Stderr, "jev: no OpenRouter API key available: set %s, or configure an \"openrouter\" credential of type \"api\" in opencode's auth store; refusing to start.\n", credentials.EnvVar)
		os.Exit(exitCode)
	}
	apiKey := cred.Key
	fmt.Fprintf(os.Stderr, "jev: using OpenRouter key from %s\n", cred.Source)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev: loading config: %v\n", err)
		os.Exit(exitCode)
	}

	auditPath, err := audit.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev: resolving audit log path: %v\n", err)
		os.Exit(exitCode)
	}
	auditLog := audit.NewLogger(auditPath)

	client := openrouter.NewClient(apiKey, openrouter.RetryPolicy{
		MaxAttempts:   cfg.Retry.MaxAttempts,
		BaseBackoffMs: cfg.Retry.BaseBackoffMs,
		MaxBackoffMs:  cfg.Retry.MaxBackoffMs,
	})

	spend := budget.NewTracker(cfg.Budget.MaxUSDPerSession)

	return &registry.Deps{
		Client: client,
		Config: cfg,
		Budget: spend,
		Audit:  auditLog,
	}
}

// newServer builds the MCP server and registers every self-registered tool
// (see internal/registry) against it via RegisterMCP. Split out from
// runMCPServer so tests can construct a server wired to a *registry.Deps
// pointed at a fake OpenRouter endpoint, and drive it through a real MCP
// client/server session (see main_test.go), without going through
// os.Exit-prone startup code or touching the real network.
func newServer(deps *registry.Deps, tools []registry.Tool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jev-cli",
		Version: serverVersion,
	}, nil)

	for _, t := range tools {
		t.RegisterMCP(server, deps)
	}

	return server
}

// mustConfigPath is used only for the startup log line; config.Load()
// above has already succeeded, and config.Path() shares its (env-var-only)
// failure mode, so an error here would indicate the environment changed
// between the two calls -- fall back to a placeholder rather than crash on
// a cosmetic log line.
func mustConfigPath() string {
	p, err := config.Path()
	if err != nil {
		return "(unresolved)"
	}
	return p
}

// mustAuditPath mirrors mustConfigPath, for the same reason: it's used
// only for the startup log line, after buildDeps has already resolved
// audit.DefaultPath() successfully once, so an error here would only
// indicate the environment changed between the two calls.
func mustAuditPath() string {
	p, err := audit.DefaultPath()
	if err != nil {
		return "(unresolved)"
	}
	return p
}
