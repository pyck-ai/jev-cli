# jev-cli

jev-cli exposes TypeSafe's **Jev** judgment model, via
[OpenRouter](https://openrouter.ai)'s SystemOne API, as 14 tools -- as an
MCP (Model Context Protocol) server for [opencode](https://opencode.ai) or
any other MCP-compatible client, **and** as a plain Unix CLI (`jev score
...`, `jev verify ...`, ...) for everything else. Same 14 tools, same
underlying logic, either way -- see
[Two run modes, one registration](docs/architecture.md#two-run-modes-one-registration).

## What is jev-cli?

Jev doesn't generate free text. It answers typed, closed-form questions and
returns calibrated probability distributions over every possible answer in
~150-500ms. Every tool here is a different framing of the same three
SystemOne primitives -- `noul` (probability a statement holds), `choice`
(probability distribution over a fixed option set), and `score` (probability
distribution over an integer scale) -- batched, composed, and thresholded
for a specific job. `jev_ask` is the escape hatch: it exposes those
primitives almost directly for anything the other 13 tools don't cover.

Out of the box, jev-cli includes:

- **14 tools** — `jev_score`, `jev_verify`, `jev_screen`, `jev_check`,
  `jev_match`, `jev_rerank`, `jev_classify`, `jev_decide`, `jev_compare`,
  `jev_extract`, `jev_review`, `jev_gate`, `jev_doctor`, and `jev_ask`. See
  [Tool reference](docs/tool-reference.md) for the full input/output spec of
  each.
- **Both an MCP server and a CLI, from one binary**: `jev` with no
  arguments (or `jev mcp`) speaks MCP over stdio; `jev score ...`, `jev
  verify ...`, etc. run the exact same tool logic as a Unix command, with
  flags or a `--json` blob for input, human-readable text or `-o json` for
  output, and an exit code reflecting the tool's own verdict — see
  [CLI usage](docs/development.md#cli-usage).
- **A [self-registering plugin architecture](docs/architecture.md#plugin-architecture)**:
  adding or removing a tool is a one-package, one-line change, and involves
  editing no other file.
- **[Fail-closed conventions](docs/architecture.md#conventions-shared-by-every-tool)**
  shared by every tool: never a fabricated verdict, thresholds with
  documented defaults, one batched SystemOne request instead of a loop.
- **Reuses opencode's own OpenRouter login** — if you're already logged into
  OpenRouter through [opencode](https://opencode.ai), jev-cli picks up that
  same key automatically, with zero extra configuration; see
  [Configuration](docs/configuration.md#api-key-required) for the exact
  fallback order and when you'd want to override it.
- **A budget-enforced, audited call path**: every call is logged to a
  JSON-lines audit file (the judged input itself is hashed, never stored
  verbatim), and per-call/session USD caps are enforced before and after
  each request — see [Configuration](docs/configuration.md).

## Getting started

Needs Go 1.25+ and an OpenRouter API key with access to the `typesafe/jev-*`
model family — see [Configuration](docs/configuration.md) for how the key is
resolved (it reuses opencode's stored OpenRouter key if there is one).

Install the `jev` binary from the `main` branch into `$(go env GOPATH)/bin`
(or `$GOBIN`); re-run the same command to update:

```sh
go install github.com/pyck-ai/jev-cli/cmd/jev@main
```

Use it as a CLI:

```sh
jev score --state "2+2=4" --scale-min 0 --scale-max 1 --instructions "0=false, 1=true"
jev --help
```

Or as an MCP server: with no arguments, `jev` speaks MCP over stdio, so an
MCP client's server command is just `jev`. See
[Using jev from an MCP client](docs/development.md#using-jev-from-an-mcp-client)
for opencode and Claude Code configs.

To run it without installing anything, use `go run`, which fetches and
builds the current `main` branch on each start:

```sh
go run github.com/pyck-ai/jev-cli/cmd/jev@main score --state "2+2=4" --scale-min 0 --scale-max 1 --instructions "0=false, 1=true"
```

To work on it from a checkout:

```sh
go build -o jev ./cmd/jev   # build the binary
go test -race ./...         # unit + fake-server integration tests
```

No OpenRouter account is needed to run the tests; every tool is tested
against a fake OpenRouter server (see [Testing](docs/development.md#testing)).
See [Development](docs/development.md) for the full workflow.

## Documentation

- [Configuration](docs/configuration.md) — API key resolution, the config
  file, environment overrides, the audit log, and budget enforcement.
- [Architecture](docs/architecture.md) — the plugin mechanism, shared
  plumbing, conventions, and package layout.
- [Development](docs/development.md) — build, test, run standalone, and
  register with opencode.
- [Tool reference](docs/tool-reference.md) — input/output tables and an
  example for every tool.

[`docs/README.md`](docs/README.md) indexes all of the above.

## Inspiration

jev-cli is a clean-room implementation (no shared code), but its design was
shaped by three existing Jev/SystemOne MCP servers, each analyzed before
writing a line of this one:

- **[jkudish/jev-mcp](https://github.com/jkudish/jev-mcp)** (TypeScript) — the
  primary influence on the tool surface. Its 11 opinionated, named tools
  (`jev_verify`, `jev_review`, `jev_gate`, and the rest) are what `jev_verify`
  through `jev_gate` here are modeled on, adapted onto SystemOne's actual wire
  format rather than its own `@jkudish/jev-agent-tools` abstraction.
- **[itsmostafa/system-one-connector](https://github.com/itsmostafa/system-one-connector)**
  (Go) — the closest architectural sibling: a single static Go binary calling
  OpenRouter directly. `jev_check` and `jev_match` are renames of its
  `noul`/`choice` primitive framing; its binary-distribution model is why this
  project builds as one Go binary too.
- **[blakestone-x/jev-mcp](https://github.com/blakestone-x/jev-mcp)**
  (Python) — TypeSafe-direct only, no OpenRouter path, but the source of
  `jev_ask` (its generic question-map escape hatch) and `jev_doctor`
  (its `jev_health` renamed).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to set up, make a change, and
find your way around.

## Status

Early (`v0.1.0`, see `cmd/jev/main.go`'s `serverVersion`). Tool schemas, defaults,
and thresholds may still change between releases.

## License

MIT. See [LICENSE](LICENSE).
