# Development

How to build jev-mcp, run its test suite, sanity-check the binary
standalone, and register it with opencode. See
[Configuration](configuration.md) for the API key, config file, and audit
log this all depends on, and [Architecture](architecture.md) for how the
tools these commands build are put together.

## Build

```sh
go build -o jev-mcp .
```

`go vet ./...`, `gofmt -l .` (no output), and `go test -race ./...` are also
clean (see [Testing](#testing)).

## Testing

```sh
go test ./...        # unit + fake-server integration tests
go test -race ./...
go vet ./...
gofmt -l .            # no output = clean
```

No `OPENROUTER_API_KEY` is required to run the test suite: every
`internal/tools/*` package (and `internal/openrouter`, `internal/tools/reviewcore`)
runs its handler tests against an `httptest.Server` standing in for
OpenRouter (see `internal/openrouter/client_test.go` and
`internal/tools/score/score_test.go` for the pattern every other tool
package follows), and `main_test.go` drives `jev_score` through a real MCP
client/server session (in-memory transport) end to end, plus a
`TestNewServer_RegistersEveryToolExactlyOnce` regression guard confirming
all 14 tools register under distinct names. **None of this exercises a
real network call to the actual OpenRouter service** — the request/response
shapes these tests assert on are transcribed from OpenRouter's published
docs and the project brief's verified-live wire facts (see
`internal/openrouter/types.go`), not confirmed against a live response.
Re-verify against a real `OPENROUTER_API_KEY` before depending on this in
anything important. `internal/credentials`' tests point `XDG_DATA_HOME` at a
temp dir (via `t.Setenv`) for every case, so they never read or touch a
real `~/.local/share/opencode/auth.json`.

## Running standalone

`jev-mcp` speaks MCP over stdio. To sanity-check it starts up correctly:

```sh
OPENROUTER_API_KEY=sk-or-... ./jev-mcp
```

You should see two banner lines on stderr — which API key source was used,
then the usual startup summary:

```
jev-mcp: using OpenRouter key from env
jev-mcp: starting (tools=14, default_model=~typesafe/jev-latest, config=..., audit_log=...)
```

(The first line reads `jev-mcp: using OpenRouter key from opencode auth
store at <path>` instead when falling back to opencode's stored credentials
— see [API key](configuration.md#api-key-required) — and never prints the key value either
way. `tools=14` counts every self-registered tool, i.e. it moves in lockstep
with the blank imports in `main.go` — see
[Plugin architecture](architecture.md#plugin-architecture); it does not name a single
resolved model, since different tools may resolve different models via
`tool_model_overrides`.) It then sits waiting for JSON-RPC frames on stdin.
`Ctrl-C` to stop. stdout is reserved exclusively for the MCP protocol stream
— all diagnostics go to stderr. With no key available from either source,
you'll instead see a one-line `no OpenRouter API key available: ...` error
and the process exits non-zero.

## Using jev-mcp from an MCP client

jev-mcp is a stdio MCP server: any MCP-compatible client can spawn it
directly. Two invocation styles work everywhere below:

- **`go run github.com/pyck-ai/jev-mcp@latest`** — no local checkout or build
  step; Go resolves, builds, and runs the module in one command. No tagged
  release is required for this to work: with no tags, `@latest` resolves to
  a pseudo-version built from the newest commit on the default branch (Go's
  own module resolution falls back this way automatically); once tags exist,
  `@latest` tracks the newest one instead. Pin `@v0.1.0` or a commit `@<sha>`
  for reproducible behavior across machines regardless. Requires the Go
  toolchain and network access to `proxy.golang.org` (or a configured
  `GOPROXY` mirror) on whatever machine runs it, and re-resolves/compiles on
  every cold start.
- **A locally built binary** (`go build -o jev-mcp .` — see [Build](#build))
  — no network or toolchain needed at runtime, lower-latency cold start.
  Prefer this for a checked-out working copy you already have.

Every example below uses `go run`; swap in a path to a locally built binary
if you prefer.

### opencode

Add to `opencode.json` (see [opencode's MCP docs](https://opencode.ai/docs/mcp-servers/)):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "jev": {
      "type": "local",
      "command": ["go", "run", "github.com/pyck-ai/jev-mcp@latest"],
      "enabled": true
    }
  }
}
```

Since jev-mcp is being registered from *inside* opencode here, the
credential fallback described under [API key](configuration.md#api-key-required) usually
means that's all you need: if you've already logged into OpenRouter through
opencode (`~/.local/share/opencode/auth.json` has a `"type": "api"`
`openrouter` entry), jev-mcp will pick that up automatically with no
`environment` block at all. Add one explicitly only if you want jev-mcp to
use a *different* OpenRouter key than the rest of opencode:

```json
{
  "mcp": {
    "jev": {
      "type": "local",
      "command": ["go", "run", "github.com/pyck-ai/jev-mcp@latest"],
      "enabled": true,
      "environment": { "OPENROUTER_API_KEY": "sk-or-..." }
    }
  }
}
```

Prefer setting `OPENROUTER_API_KEY` in your shell/secret manager over
hardcoding it in `opencode.json` if you use the explicit form and that file
is checked into version control.

### Claude Code

Register a user-scoped stdio server with `claude mcp add`:

```sh
claude mcp add --transport stdio jev -- go run github.com/pyck-ai/jev-mcp@latest
```

Or add it directly to a project's `.mcp.json` for team-wide, version-controlled
config:

```json
{
  "mcpServers": {
    "jev": {
      "type": "stdio",
      "command": "go",
      "args": ["run", "github.com/pyck-ai/jev-mcp@latest"]
    }
  }
}
```

Claude Code has no OpenRouter-credential store of its own to fall back to
the way opencode does — set `OPENROUTER_API_KEY` in your shell/secret
manager (see [API key](configuration.md#api-key-required)), or add an `"env"`
block to the server entry above:

```json
{
  "mcpServers": {
    "jev": {
      "type": "stdio",
      "command": "go",
      "args": ["run", "github.com/pyck-ai/jev-mcp@latest"],
      "env": { "OPENROUTER_API_KEY": "sk-or-..." }
    }
  }
}
```
