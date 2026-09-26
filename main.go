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
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

	// The API key is resolved here (env var, falling back to opencode's own
	// stored credentials -- see internal/credentials) and passed around
	// out-of-band from *config.Config: per the project's security
	// requirements it must never be read from the jev-mcp config file,
	// logged, or included in any error message, from either source. A key
	// is required at startup -- fail fast with a clear error rather than
	// deferring the failure to the first tool call.
	cred, ok := credentials.Resolve()
	if !ok {
		fmt.Fprintf(os.Stderr, "jev-mcp: no OpenRouter API key available: set %s, or configure an \"openrouter\" credential of type \"api\" in opencode's auth store; refusing to start.\n", credentials.EnvVar)
		os.Exit(1)
	}
	apiKey := cred.Key
	fmt.Fprintf(os.Stderr, "jev-mcp: using OpenRouter key from %s\n", cred.Source)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: loading config: %v\n", err)
		os.Exit(1)
	}

	auditPath, err := audit.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: resolving audit log path: %v\n", err)
		os.Exit(1)
	}
	auditLog := audit.NewLogger(auditPath)

	client := openrouter.NewClient(apiKey, openrouter.RetryPolicy{
		MaxAttempts:   cfg.Retry.MaxAttempts,
		BaseBackoffMs: cfg.Retry.BaseBackoffMs,
		MaxBackoffMs:  cfg.Retry.MaxBackoffMs,
	})

	spend := budget.NewTracker(cfg.Budget.MaxUSDPerSession)

	deps := &registry.Deps{
		Client: client,
		Config: cfg,
		Budget: spend,
		Audit:  auditLog,
	}

	server := newServer(deps)

	// Unlike before this file supported more than one tool, the startup
	// log line can no longer name a single tool's resolved model (each
	// registered tool may resolve a different model via
	// cfg.ToolModelOverrides) -- it reports the tool count and the
	// fallback default_model instead.
	fmt.Fprintf(os.Stderr, "jev-mcp: starting (tools=%d, default_model=%s, config=%s, audit_log=%s)\n",
		len(registry.All()), cfg.DefaultModel, mustConfigPath(), auditPath)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "jev-mcp: server exited with error: %v\n", err)
		os.Exit(1)
	}
}

// newServer builds the MCP server and registers every self-registered tool
// (see internal/registry) against it. Split out from main() so tests can
// construct a server wired to a *registry.Deps pointed at a fake
// OpenRouter endpoint, and drive it through a real MCP client/server
// session (see main_test.go), without going through os.Exit-prone startup
// code or touching the real network.
func newServer(deps *registry.Deps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "jev-mcp",
		Version: serverVersion,
	}, nil)

	for _, register := range registry.All() {
		register(server, deps)
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
