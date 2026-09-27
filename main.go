// Command jev-mcp is an MCP (Model Context Protocol) server exposing
// TypeSafe's "Jev" judgment model, via OpenRouter's SystemOne API, as a set
// of MCP tools. See README.md for setup, configuration, and the tool
// reference.
//
// Tools are self-registering plugins (see internal/registry's package doc
// comment for the full mechanism): this file has no knowledge of any
// specific tool beyond the blank imports below, which is what actually
// activates each one. Adding a tool = one new package under
// internal/tools/ plus one new blank-import line here; removing a tool =
// delete that package plus its blank-import line. No other code in this
// file changes either way.
//
// This binary mode-switches on its first argument (see main): with no
// arguments, or "mcp" as the first argument, it runs as an MCP server
// (runMCPServer, unchanged from before this file supported any other
// mode). Any other first argument runs it as a CLI instead (runCLI) --
// scaffolding for now, since every tool's registry.Tool.RegisterCLI is
// nil today; a later pass will populate it per tool.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/credentials"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"

	_ "github.com/pyck-ai/jev-mcp/internal/tools/ask"      // jev_ask
	_ "github.com/pyck-ai/jev-mcp/internal/tools/check"    // jev_check
	_ "github.com/pyck-ai/jev-mcp/internal/tools/classify" // jev_classify
	_ "github.com/pyck-ai/jev-mcp/internal/tools/compare"  // jev_compare
	_ "github.com/pyck-ai/jev-mcp/internal/tools/decide"   // jev_decide
	_ "github.com/pyck-ai/jev-mcp/internal/tools/doctor"   // jev_doctor
	_ "github.com/pyck-ai/jev-mcp/internal/tools/extract"  // jev_extract
	_ "github.com/pyck-ai/jev-mcp/internal/tools/gate"     // jev_gate
	_ "github.com/pyck-ai/jev-mcp/internal/tools/match"    // jev_match
	_ "github.com/pyck-ai/jev-mcp/internal/tools/rerank"   // jev_rerank
	_ "github.com/pyck-ai/jev-mcp/internal/tools/review"   // jev_review
	_ "github.com/pyck-ai/jev-mcp/internal/tools/score"    // jev_score
	_ "github.com/pyck-ai/jev-mcp/internal/tools/screen"   // jev_screen
	_ "github.com/pyck-ai/jev-mcp/internal/tools/verify"   // jev_verify
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
		fmt.Fprintf(os.Stderr, "jev-mcp: warning: could not set JSONSCHEMAGODEBUG: %v\n", err)
	}

	// Mode dispatch. With no arguments, or "mcp" as the first argument,
	// run the MCP server exactly as this binary always has
	// (runMCPServer). Any other first argument switches to CLI mode
	// instead (runCLI): a cobra root command is built, and every
	// registered tool's RegisterCLI hook -- see internal/registry's
	// package doc comment -- gets a chance to add itself as a
	// subcommand. This is scaffolding only: every tool's RegisterCLI is
	// nil today, so the CLI root command currently has zero subcommands;
	// a later pass will populate RegisterCLI per tool.
	if len(os.Args) > 1 && os.Args[1] != "mcp" {
		runCLI()
		return
	}
	runMCPServer()
}

// runMCPServer is jev-mcp's default run mode: build the shared
// dependencies, register every tool as an MCP tool, and serve MCP over
// stdio until the client disconnects or an error occurs. Unchanged from
// before this binary supported any other mode, except that its
// tool-registration loop (inside newServer) now invokes each
// registry.Tool's RegisterMCP field instead of calling a bare
// registry.Registrar function value -- that type no longer exists, see
// internal/registry's package doc comment.
func runMCPServer() {
	deps := buildDeps(1)

	server := newServer(deps)

	// Unlike before this file supported more than one tool, the startup
	// log line can no longer name a single tool's resolved model (each
	// registered tool may resolve a different model via
	// cfg.ToolModelOverrides) -- it reports the tool count and the
	// fallback default_model instead.
	fmt.Fprintf(os.Stderr, "jev-mcp: starting (tools=%d, default_model=%s, config=%s, audit_log=%s)\n",
		len(registry.All()), deps.Config.DefaultModel, mustConfigPath(), mustAuditPath())

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: server exited with error: %v\n", err)
		os.Exit(1)
	}
}

// runCLI is jev-mcp's CLI run mode: build a cobra root command and let
// every registered tool's RegisterCLI hook add itself as a subcommand,
// skipping the tools whose RegisterCLI is nil (all 14 of them, today),
// then execute it against the process's actual arguments (cobra reads
// os.Args itself here -- root.Execute is never given an explicit
// SetArgs). This is scaffolding for a later pass that will actually
// populate RegisterCLI per tool: today it always yields a root command
// with zero subcommands.
//
// The deps handed to RegisterCLI are a LAZY provider, deliberately not a
// built *Deps: building them resolves the OpenRouter credential, the
// config file, and the audit log, and fail-fasts the process if any is
// missing. Deferring that until a subcommand actually runs (the provider
// is only invoked from a command's RunE, never during flag parsing) is
// what lets `jev --help`, `jev score --help`, and cobra's unknown-flag
// errors work on a machine with no credentials configured at all.
func runCLI() {
	// NoArgs + a help-printing RunE make the root behave sanely at this
	// intermediate stage (no subcommands registered yet): `jev --help`
	// prints help and exits 0, and an unrecognized first argument (e.g.
	// `jev score` before score's RegisterCLI exists) produces cobra's
	// "unknown command" error instead of cobra v1.10's silent no-op for
	// a non-runnable, childless root (verified empirically against
	// v1.10.2: without this, both cases print nothing and exit 0).
	root := &cobra.Command{
		Use:  "jev",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	for _, t := range registry.All() {
		if t.RegisterCLI != nil {
			t.RegisterCLI(root, func() *registry.Deps { return buildDeps(3) })
		}
	}

	// Exit 3 on any Execute error (unknown command, bad flags): the CLI
	// exit-code scheme reserves 3 for hard errors, 0/1/2 for verdict
	// outcomes.
	if err := root.Execute(); err != nil {
		os.Exit(3)
	}
}

// buildDeps resolves the API key and config and builds the shared
// *registry.Deps every tool's RegisterMCP/RegisterCLI needs -- identically
// for both of jev-mcp's run modes. This is exactly the construction
// sequence this file always had before it supported any mode but the MCP
// server, factored out so runCLI can share it rather than duplicating it.
//
// The API key is resolved here (env var, falling back to opencode's own
// stored credentials -- see internal/credentials) and passed around
// out-of-band from *config.Config: per the project's security
// requirements it must never be read from the jev-mcp config file,
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
		fmt.Fprintf(os.Stderr, "jev-mcp: no OpenRouter API key available: set %s, or configure an \"openrouter\" credential of type \"api\" in opencode's auth store; refusing to start.\n", credentials.EnvVar)
		os.Exit(exitCode)
	}
	apiKey := cred.Key
	fmt.Fprintf(os.Stderr, "jev-mcp: using OpenRouter key from %s\n", cred.Source)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: loading config: %v\n", err)
		os.Exit(exitCode)
	}

	auditPath, err := audit.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: resolving audit log path: %v\n", err)
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
func newServer(deps *registry.Deps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jev-mcp",
		Version: serverVersion,
	}, nil)

	for _, t := range registry.All() {
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
