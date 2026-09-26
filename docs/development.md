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

## Registering with opencode

Add to `opencode.json` (see [opencode's MCP docs](https://opencode.ai/docs/mcp-servers/)):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "jev": {
      "type": "local",
      "command": ["/home/you/src/local/jev-mcp/jev-mcp"],
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
      "command": ["/home/you/src/local/jev-mcp/jev-mcp"],
      "enabled": true,
      "environment": { "OPENROUTER_API_KEY": "sk-or-..." }
    }
  }
}
```

Adjust the `command` path to wherever you built the binary. Prefer setting
`OPENROUTER_API_KEY` in your shell/secret manager over hardcoding it in
`opencode.json` if you use the explicit form and that file is checked into
version control.

### Running via `go run` instead of a built binary

Once this repo is pushed to `github.com/pyck-ai/jev-mcp`, no separate build
step is required — `go run` fetches, builds, and runs the module in one
command:

```sh
go run github.com/pyck-ai/jev-mcp@latest
```

`@latest` resolves to the newest tagged release; pin a specific version
(`@v0.1.0`) or commit (`@<sha>`) instead if you want reproducible behavior
across machines. In `opencode.json`, this replaces the `command` array with
the `go run` invocation instead of a path to a locally built binary:

```json
{
  "mcp": {
    "jev": {
      "type": "local",
      "command": ["go", "run", "github.com/pyck-ai/jev-mcp@latest"],
      "enabled": true
    }
  }
}
```

The tradeoff: `go run` re-resolves and compiles on every cold start (a local
build only pays that cost once), and it requires the Go toolchain and
network access to `proxy.golang.org` (or `GOPROXY`/`GOFLAGS=-mod=mod`
configured for a private/offline mirror) on whatever machine runs it. Prefer
a locally built binary — see [Build](#build) — for a lower-latency, offline
startup; prefer `go run` when you want to track a version without a manual
rebuild step, or run it on a machine that never has this repo checked out
locally at all.
version control.
