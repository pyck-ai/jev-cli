# Architecture

How jev-cli's tools are structured as self-registering plugins, the shared
plumbing every tool reuses, the conventions every tool follows, and the
repository's package layout. See [Tool reference](tool-reference.md) for
the per-tool input/output spec this architecture supports, and
[Configuration](configuration.md) for the config/audit/budget plumbing
referenced below.

## Plugin architecture

Every tool is a self-registering plugin, modeled directly on Go's
`database/sql` driver pattern (`import _ "github.com/lib/pq"`):

- Each tool lives in its own package under `internal/tools/<name>/`.
- That package's `init()` function calls `registry.Register(...)` with a
  `registry.Tool`: `Name` (bare CLI subcommand name, e.g. `"score"`),
  `MCPName` (e.g. `"jev_score"`), `Description`, a `RegisterMCP` closure
  that builds the tool's handler from `*registry.Deps` (the shared
  `Client`/`Config`/`Budget`/`Audit` infrastructure) and calls
  `mcp.AddTool` against whatever `*mcp.Server` it's given, and a
  `RegisterCLI` closure that does the equivalent for a `*cobra.Command`
  (every one of the 14 tools populates both today -- see
  [CLI mode](#cli-mode) below).
- `cmd/jev/main.go` activates the whole tool set with one blank import per tool
  package (`_ "github.com/pyck-ai/jev-cli/internal/tools/<name>"`), builds
  one `*registry.Deps`, then loops `for _, t := range registry.All() {
  t.RegisterMCP(server, deps) }` before starting the server.

### CLI mode

`jev-cli`'s binary mode-switches on its first argument: with no arguments,
or `mcp` as the first argument, it runs the MCP server described above,
unchanged. Any other first argument instead builds a `cobra` root command
(`Use: "jev"`) and loops over `registry.All()` calling `t.RegisterCLI(root,
provider)` for every tool, where `provider` is a *lazy* `func()
*registry.Deps` — invoked only from a subcommand's actual run path, never
during flag parsing or help, so `jev --help` and `jev <tool> --help` work
with no credentials configured at all.

Every tool's `RegisterCLI` follows the same shape (see
`internal/tools/score/cli.go` for the reference implementation any new
tool's `cli.go` should copy):

- **Input**: `internal/cliinput.Bind[Input](cmd)` registers one flag per
  `Input` field (JSON tag, snake_case → kebab-case), plus the shared
  `--json`/`-j` flag for supplying the whole input as one JSON object (or
  reading it from stdin via `-j -`). A structured field (slice, map,
  nested struct, or a `string`-or-JSON field like `jev_ask`'s `state`)
  takes its flag value as JSON text, not a bare literal — see
  [Tool reference](tool-reference.md) for which fields need this per tool.
  Required-field validation is NOT duplicated in `cliinput`: each tool's
  own `run` (the same core the MCP handler calls) rejects missing/invalid
  input with its own precise error message, under either input path.
- **Core logic**: the CLI adapter calls the exact same `run(ctx, in)
  (Output, error)` the MCP handler calls — see
  [Two run modes, one registration](#two-run-modes-one-registration)
  below.
- **Output**: `internal/cliformat.Emit` renders `Output` as aligned
  human-readable text by default, or `json.MarshalIndent`-equivalent JSON
  with `-o json` — see that package's doc comment for the exact text
  layout (aligned `field: value` lines, sorted map entries, a
  `text/tabwriter` table for slice-of-struct fields, recursive rendering
  for nested structs/maps so no field ever falls through to Go's raw
  `%v` struct dump, which would risk printing an unexported field or a
  live pointer address).
- **Exit code**: a small per-tool `exitCode(Output) int` maps that
  specific tool's verdict field(s) onto the CLI's shared four-value
  scheme — see [Exit codes](development.md#exit-codes).

### Two run modes, one registration

Every tool's handler type has a `run(ctx context.Context, in Input)
(Output, error)` method: everything from input validation through the
SystemOne call, budget accounting, and audit logging, independent of the
caller. `Handle` (the MCP-shaped method `mcp.AddTool` is given) and the
CLI adapter's `RunE` are both thin wrappers around this same `run` — the
MCP-vs-CLI split happens only in how the input arrives and the output
leaves, never in the tool's own logic. This is what keeps `RegisterMCP`
and `RegisterCLI` from drifting apart: there is exactly one place either
one's tests can catch a behavior regression.

**Adding a tool**: create `internal/tools/<name>/<name>.go` (package
`<name>`) whose `init()` registers itself — see `internal/tools/check`'s
`check.go` for a compact reference implementation, or
`internal/tools/score`'s `score.go` for the original tool this architecture
was extracted from. Then add one blank-import line to `cmd/jev/main.go`.

**Removing a tool**: delete that package directory, then delete its
blank-import line from `cmd/jev/main.go`.

**No other file needs to change either way** — that is the entire point of
`internal/registry` (see that package's own doc comment for the full
mechanism and its concurrency argument).

Shared plumbing every tool reuses rather than reimplementing:

| Package | What it provides |
|---|---|
| `internal/openrouter` | HTTP client: retry/backoff/timeout, and `Client.Ask`, which sends an arbitrary map of named questions (mixed `noul`/`choice`/`score` types) in one SystemOne request. |
| `internal/answers` | Fail-closed parsing for `noul`/`choice`/`score` answers (`Noul`, `Choice`, `Score`), `NoulLabel` (likely/unlikely/uncertain), and `ResolveThreshold`/`ValidateAutoAccept` for the `auto_accept`-style input fields most tools expose. |
| `internal/capstring` | Rune-count text truncation for every "capped at N characters" input field. |
| `internal/budget` | Session spend tracking + pre-call refusal (shared by every tool's handler). |
| `internal/audit` | JSONL audit log (`audit.Entry`, `audit.HashValue`, including `ItemCount`/`InvalidCount` for batched tools — see [Audit log](configuration.md#audit-log)). |
| `internal/tools/reviewcore` | The four-rubric scoring/composite/action logic shared by `jev_review` and `jev_gate`. |

### Conventions shared by every tool

- **Fail-closed, always.** Every atomic judgment (a claim, proposition,
  candidate, item, field, requirement check, rubric, ...) carries a status
  that is either `"ok"`, or `"invalid_response"` (sometimes folded directly
  into a richer tool-specific enum, e.g. `jev_extract`'s field `status`) —
  never a fabricated verdict. A malformed/missing item always forces its
  tool's most conservative action (`review` rather than `auto`; `escalate`
  rather than `review`, where both exist).
- **Thresholds are optional input fields with documented defaults.** Every
  `auto_accept`/`composite_floor`/`block_at`/... field is resolved against
  its default when omitted or `0` (see `answers.ResolveThreshold`);
  `auto_accept`-style confidence bars must be `> 0.5` if set explicitly
  (see `answers.ValidateAutoAccept`).
- **Size caps: truncate a single field, reject a list/aggregate.** A single
  text field capped at N characters (e.g. `jev_compare`'s `passage_a`) is
  silently truncated (rune-counted, via `capstring.Truncate`). A list capped
  at N items, or an aggregate character cap across a whole list (e.g.
  `jev_rerank`'s 100,000-character aggregate), instead **rejects the call
  outright** with a clear validation error: silently dropping or truncating
  an unspecified subset of a list would change the very result the tool
  produces, in a way a caller could easily miss.
- **Batch, don't loop.** A tool judging N independent things asks one named
  question per thing in a **single** SystemOne HTTP request — never one
  round trip per item.
- **Every question is self-sufficient.** In a batched or composite call,
  each named question's `instructions` (plus the request's one shared
  `state`) contains everything needed to answer it — no question relies on
  being able to see a sibling question's own `criteria`/`instructions`
  within the same request, since that cross-visibility is not a documented
  or verified SystemOne behavior.
- **Same budget/audit plumbing as `jev_score`, every time.** Pre-call
  session-budget refusal, post-hoc per-call `budget_exceeded` flagging from
  `usage.cost`, and one `audit.Entry` per call.

## Project layout

```
go.mod / go.sum
cmd/jev/main.go                // the jev binary: run-mode switch, blank-imports every tool package, API key resolution + fail-fast
cmd/jev/main_test.go           // end-to-end MCP wire-protocol test + MCP and CLI roster guards
internal/config/                // config file loading + env overrides
internal/credentials/           // OpenRouter API key resolution: env var, then opencode's auth store
internal/openrouter/            // HTTP client, retry/backoff, SystemOne request/response shapes, Client.Ask
internal/audit/                 // JSONL audit writer
internal/budget/                // session spend tracking
internal/answers/                // shared noul/choice/score answer parsing + threshold helpers
internal/capstring/              // rune-count text truncation
internal/registry/               // the self-registering tool-plugin mechanism (see Plugin architecture)
internal/xdg/                   // shared $XDG_DATA_HOME resolution (used by audit + credentials)
internal/tools/
  score/                        // jev_score
  verify/                       // jev_verify
  screen/                       // jev_screen
  check/                        // jev_check
  match/                        // jev_match
  rerank/                       // jev_rerank
  classify/                     // jev_classify
  decide/                       // jev_decide
  compare/                      // jev_compare
  extract/                      // jev_extract
  review/                       // jev_review
  gate/                         // jev_gate (built on reviewcore, alongside review)
  reviewcore/                   // scoring/composite/action logic shared by review + gate
  doctor/                       // jev_doctor
  ask/                          // jev_ask
```
