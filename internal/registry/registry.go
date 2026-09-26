// Package registry implements jev-mcp's self-registering tool-plugin
// mechanism, modeled directly on Go's database/sql driver pattern (e.g.
// `import _ "github.com/lib/pq"`): each MCP tool lives in its own package
// under internal/tools/<name>/, and that package's init() function calls
// Register to record a Registrar. main.go activates the whole tool set
// with one blank import per tool package plus a single loop over All().
//
// # Adding or removing a tool
//
// Adding a tool: create a new package under internal/tools/<name>/ whose
// init() calls registry.Register(...) (see internal/tools/score/score.go
// for the reference implementation), then add one blank-import line to
// main.go: `_ "github.com/pyck-ai/jev-mcp/internal/tools/<name>"`.
//
// Removing a tool: delete that package directory, then delete its
// blank-import line from main.go. No other file needs to change either
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
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
)

// Deps is the shared application infrastructure every tool handler may
// need, built once in main.go from real dependencies (or once per test
// from fakes/httptest servers) and passed to every registered Registrar.
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

// Registrar is called once at server startup for every tool package that
// registered itself via Register. A Registrar should call mcp.AddTool
// (possibly more than once, for a package exposing more than one MCP
// tool) against server, building whatever per-tool handler it needs from
// deps rather than constructing its own client/config/budget/audit.
type Registrar func(server *mcp.Server, deps *Deps)

// registrars accumulates every Registrar passed to Register, in call
// order. See the package doc comment ("Concurrency") for why this is safe
// without a mutex.
var registrars []Registrar

// Register records r to be invoked later by whatever calls All() (in
// practice, main() exactly once at startup). Intended to be called only
// from a tool package's init() function -- see the package doc comment.
func Register(r Registrar) {
	registrars = append(registrars, r)
}

// All returns every Registrar recorded so far via Register, in
// registration order. Because Register is only ever called from package
// init() functions, that order is Go's own package initialization order
// for whichever set of tool packages main.go blank-imports.
func All() []Registrar {
	return registrars
}
