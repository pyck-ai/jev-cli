// Package registry implements jev-cli's self-registering tool-plugin
// mechanism, modeled directly on Go's database/sql driver pattern (e.g.
// `import _ "github.com/lib/pq"`): each MCP tool lives in its own package
// under internal/tools/<name>/, and that package's init() function calls
// Register to record a Tool. cmd/jev/main.go activates the whole tool set with
// one blank import per tool package plus a single loop over All().
//
// # Two run modes, one registration
//
// A Tool carries both a RegisterMCP hook (wiring it onto an *mcp.Server,
// for `jev mcp`) and a RegisterCLI hook (adding it as a subcommand of the
// *cobra.Command root, for `jev <name> ...`). Every tool sets both hooks,
// and both call the same core function, so the two modes share one
// implementation. cmd/jev/main.go skips a Tool whose RegisterCLI is nil,
// so a tool can still be MCP-only if it ever needs to be.
//
// # Adding or removing a tool
//
// Adding a tool: create a new package under internal/tools/<name>/ whose
// init() calls registry.Register(...) (see internal/tools/score/score.go
// for the reference implementation), then add one blank-import line to
// cmd/jev/main.go: `_ "github.com/pyck-ai/jev-cli/internal/tools/<name>"`.
//
// Removing a tool: delete that package directory, then delete its
// blank-import line from cmd/jev/main.go. No other file needs to change either
// way -- that is the entire point of this package.
//
// # Concurrency
//
// Register is only ever called from package init() functions, which Go
// runs single-threaded, in dependency order, before main() begins --
// strictly before the MCP server (and any of its goroutines) is started.
// All() is only ever called once, from main(), after every package's
// init() has already run and before the server starts serving requests.
// There is consequently no concurrent access to the underlying slice at
// any point in this program's lifetime, so this package uses a plain
// slice with no mutex -- unlike, say, internal/budget.Tracker, which
// genuinely is shared across concurrent tool calls once the server is
// running.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// Deps is the shared application infrastructure every tool handler may
// need, built once in cmd/jev/main.go from real dependencies (or once per test
// from fakes/httptest servers) and passed to every registered Tool's
// RegisterMCP (and, once populated, RegisterCLI).
//
// Field types are copied verbatim from how each dependency was already
// threaded through the codebase before this package existed (see the
// pre-registry tools.NewScoreHandler constructor), not guessed from the
// illustrative sketch in the project brief that introduced this type:
//   - Config is config.Config BY VALUE, not *config.Config: config.Load
//     already returns a Config by value, and config.Config.ModelForTool
//     already has a value receiver (see internal/config/config.go), so
//     every existing call site already treats Config as a small,
//     read-only value type, not a pointer.
//   - Budget is *budget.Tracker: budget.NewTracker returns one, and it
//     must be a pointer regardless (its mutex and running total need to
//     be shared across every tool call in the process).
//   - Audit is *audit.Logger, not *audit.Writer: the audit package's
//     exported type is named Logger.
//   - Client is *openrouter.Client, matching openrouter.NewClient's
//     return type.
type Deps struct {
	Client *openrouter.Client
	Config config.Config
	Budget *budget.Tracker
	Audit  *audit.Logger
}

// DepsProvider lazily builds the shared *Deps, exiting the process on any
// failure. See Tool.RegisterCLI for why RegisterCLI receives a provider
// rather than a pre-built *Deps.
type DepsProvider func() *Deps

// Tool is what a tool package's init() passes to Register: everything
// cmd/jev/main.go needs to activate that tool in either of jev-cli's run modes,
// without cmd/jev/main.go knowing anything about the tool itself.
type Tool struct {
	// Name is this tool's CLI subcommand name, e.g. "score" -- bare, no
	// "jev_" prefix, matching its internal/tools/<name>/ package
	// directory.
	Name string
	// MCPName is this tool's MCP tool name, e.g. "jev_score" -- what
	// RegisterMCP registers it as via mcp.AddTool, and the key used in
	// config.Config.ToolModelOverrides and audit.Entry.Tool.
	MCPName string
	// Description is this tool's one-paragraph description, shared
	// verbatim between its MCP tool registration (mcp.Tool.Description,
	// set inside RegisterMCP) and, once RegisterCLI is populated, its CLI
	// subcommand help text.
	Description string
	// RegisterMCP wires this tool onto server as an MCP tool. It should
	// call mcp.AddTool (possibly more than once, for a tool package
	// exposing more than one MCP tool) against server, building whatever
	// per-tool handler it needs from deps rather than constructing its
	// own client/config/budget/audit. Called once per tool, in
	// registration order, by cmd/jev/main.go's MCP-server run mode.
	RegisterMCP func(server *mcp.Server, deps *Deps)
	// RegisterCLI wires this tool onto root as a CLI subcommand. nil for
	// a tool that has no CLI subcommand yet -- cmd/jev/main.go's CLI run mode
	// skips any Tool whose RegisterCLI is nil (true of every one of the
	// 14 tools registered today; see the package doc comment).
	//
	// deps is a LAZY provider, not a built *Deps: it is invoked only when
	// the subcommand actually runs, never during cobra's flag parsing or
	// help paths, so `jev --help` (and cobra's own error messages) work
	// with no OpenRouter credentials, config, or audit log present. The
	// provider itself exits the process on failure (see cmd/jev/main.go's
	// buildDeps), so a subcommand's RunE can treat its result as ready.
	RegisterCLI func(root *cobra.Command, deps DepsProvider)
	// Run executes one invocation of this tool from raw JSON input (the
	// same object an MCP client or `-j` would pass) and returns its output,
	// its CLI exit code and any error. Build it with Runner. nil means the
	// tool cannot be run through jev_batch (doctor, batch itself, and any
	// tool not yet wired).
	Run RunFunc
}

// RunFunc runs one tool invocation from raw JSON input. A non-nil err means
// the run failed (invalid input, refused, transport error) and out is nil;
// exitCode is then 3. Otherwise exitCode is the tool's own output-to-exit-code
// mapping (0 good, 1 review, 2 escalate, 3 hard failure).
type RunFunc func(ctx context.Context, deps *Deps, input json.RawMessage) (out any, exitCode int, err error)

// Runner adapts a tool's typed run core plus its exit-code mapping into a
// RunFunc. Input is validated exactly as on the MCP path: against the JSON
// schema the go-sdk reflects from I (required fields, types, no unknown
// properties, defaults applied), then decoded into I. The schema is built
// lazily on first use, so JSONSCHEMAGODEBUG set in main() is honored.
func Runner[I, O any](run func(ctx context.Context, deps *Deps, in I) (O, error), exitCode func(O) int) RunFunc {
	var (
		once     sync.Once
		resolved *jsonschema.Resolved
		schemaEr error
	)
	return func(ctx context.Context, deps *Deps, raw json.RawMessage) (any, int, error) {
		once.Do(func() {
			rt := reflect.TypeFor[I]()
			if rt.Kind() == reflect.Pointer {
				rt = rt.Elem()
			}
			var s *jsonschema.Schema
			if s, schemaEr = jsonschema.ForType(rt, &jsonschema.ForOptions{}); schemaEr == nil {
				resolved, schemaEr = s.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
			}
		})
		if schemaEr != nil {
			return nil, 3, fmt.Errorf("input schema: %w", schemaEr)
		}

		args := make(map[string]any)
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, 3, fmt.Errorf("unmarshaling arguments: %w", err)
			}
		}
		var tree any = args
		if err := resolved.ApplyDefaults(&tree); err != nil {
			return nil, 3, fmt.Errorf("applying schema defaults: %w", err)
		}
		if err := resolved.Validate(&tree); err != nil {
			return nil, 3, fmt.Errorf("validating \"arguments\": %w", err)
		}
		withDefaults, err := json.Marshal(tree)
		if err != nil {
			return nil, 3, fmt.Errorf("marshalling arguments: %w", err)
		}
		var in I
		if err := json.Unmarshal(withDefaults, &in); err != nil {
			return nil, 3, fmt.Errorf("parsing input: %w", err)
		}
		out, err := run(ctx, deps, in)
		if err != nil {
			return nil, 3, err
		}
		return out, exitCode(out), nil
	}
}

// tools accumulates every Tool passed to Register, in call order. See the
// package doc comment ("Concurrency") for why this is safe without a
// mutex.
var tools []Tool

// Register records t to be invoked later by whatever calls All() (in
// practice, main() exactly once at startup). Intended to be called only
// from a tool package's init() function -- see the package doc comment.
func Register(t Tool) {
	tools = append(tools, t)
}

// All returns every Tool recorded so far via Register, in registration
// order. Because Register is only ever called from package init()
// functions, that order is Go's own package initialization order for
// whichever set of tool packages cmd/jev/main.go blank-imports.
func All() []Tool {
	return tools
}

// Lookup returns the registered Tool whose CLI name ("decide") or MCP name
// ("jev_decide") equals name.
func Lookup(name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name || t.MCPName == name {
			return t, true
		}
	}
	return Tool{}, false
}
