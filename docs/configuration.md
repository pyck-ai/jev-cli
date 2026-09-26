# Configuration

Where jev-mcp gets its OpenRouter API key, the optional config file and the
environment variables that override it, the audit log every tool call
writes, and how per-call/session budget caps are enforced. See
[Development](development.md) for build/test/run commands, and
[Architecture](architecture.md) for how this plumbing is wired into every
tool.

## API key (required)

jev-mcp resolves the OpenRouter API key it uses at startup, in this order:

1. **`OPENROUTER_API_KEY` environment variable**, if set and non-empty:

   ```sh
   export OPENROUTER_API_KEY=sk-or-...
   ```

2. **Otherwise, opencode's own stored credentials.** If you already use
   [opencode](https://opencode.ai) and have logged into OpenRouter there,
   jev-mcp will reuse that key instead of requiring a separate one. It reads
   (never writes, never requires opencode to be running) opencode's static
   credential file at `$XDG_DATA_HOME/opencode/auth.json`, falling back to
   `~/.local/share/opencode/auth.json` when `XDG_DATA_HOME` is unset. Only an
   `"openrouter"` entry with `"type": "api"` is usable this way:
   ```json
   { "openrouter": { "type": "api", "key": "sk-or-..." } }
   ```
   An oauth-based opencode login for OpenRouter, if that's ever a thing
   (`"type": "oauth"`, like the `anthropic` entry in opencode's own example),
   is **not** usable this way and is skipped — jev-mcp only ever sends a
   plain `Authorization: Bearer <key>` header, it doesn't implement OAuth. A
   missing, unreadable, or malformed `auth.json` (bad JSON, wrong shape,
   permission denied) is treated the same as "no key here" and simply falls
   through, never a crash.

If neither source yields a key, the server fails fast at startup with a
clear message on stderr (see [Running standalone](development.md#running-standalone) for
what that looks like) — it never silently starts with no auth. Regardless
of which source is used, the resolved key is never read from jev-mcp's own
config file below, never logged, and never included in an error message; a
startup log line on stderr does say *which source* was used (e.g. `using
OpenRouter key from env` or `using OpenRouter key from opencode auth store
at <path>`), for debugging, but never the key value itself. `jev_doctor`
also reports this same source string, per-call, as part of its
configuration snapshot (see [`jev_doctor`](tool-reference.md#the-jev_doctor-tool)).

## Config file (optional)

Path: `~/.config/jev-mcp/config.json` (more precisely: `$XDG_CONFIG_HOME` or
`~/.config` if unset, joined with `jev-mcp/config.json` — this is Go's
standard `os.UserConfigDir()`). Override the path with `JEV_MCP_CONFIG_PATH`.
The file is entirely optional; if missing, the defaults below are used
as-is. If present, any field you omit keeps its default value.

```json
{
  "default_model": "typesafe/jev-latest",
  "tool_model_overrides": { "jev_score": "typesafe/jev-latest" },
  "budget": { "max_usd_per_call": 0.01, "max_usd_per_session": 1.0 },
  "retry": { "max_attempts": 3, "base_backoff_ms": 500, "max_backoff_ms": 8000 },
  "request_timeout_ms": 30000
}
```

| Field | Meaning |
|---|---|
| `default_model` | OpenRouter model slug used when a tool has no entry in `tool_model_overrides`. |
| `tool_model_overrides` | Per-tool model slug overrides, keyed by tool name (any of the 14 tool names, e.g. `jev_review`, `jev_gate`, ...). |
| `budget.max_usd_per_call` | Per-call cost cap in USD. `<= 0` means unlimited. See [Budget enforcement](#budget-enforcement). |
| `budget.max_usd_per_session` | Cumulative cost cap in USD for this server process's lifetime. `<= 0` means unlimited. |
| `retry.max_attempts` | Total HTTP attempts per tool call (initial attempt + retries). |
| `retry.base_backoff_ms` / `retry.max_backoff_ms` | Jittered exponential backoff bounds between retries. |
| `request_timeout_ms` | Total deadline for a tool call, covering **all** retry attempts combined, not per-attempt. |

### Environment overrides

| Variable | Effect |
|---|---|
| `JEV_MCP_MODEL` | Overrides `default_model`. A tool-specific entry in `tool_model_overrides` still wins over this for that tool — see the doc comment on `config.applyEnvOverrides` in `internal/config/config.go` for the precedence rationale. |
| `JEV_MCP_CONFIG_PATH` | Overrides the config file path. |

## Audit log

Every tool call appends one JSON line to
`$XDG_DATA_HOME/jev-mcp/audit.jsonl` (falling back to
`~/.local/share/jev-mcp/audit.jsonl` when `XDG_DATA_HOME` is unset). Parent
directories are created automatically. The judged input is **never** stored
verbatim: `jev_score` (whose input is a single `state` string) hashes that
string directly (`input_state_sha256`); every other tool hashes its whole
typed input struct instead (`internal/audit.HashValue`), so the digest still
changes with any part of a multi-field request even though there's no single
"state" string for those tools. Example line (`jev_score`):

```json
{"timestamp":"2026-09-26T18:00:00Z","tool":"jev_score","model":"typesafe/jev-1.13-20260917","input_state_sha256":"a94a8fe5...","scale_min":0,"scale_max":2,"score":1.99,"confidence":0.99,"status":"ok","usage":{"input_tokens":476,"output_tokens":70},"cost_usd":0.000019992,"latency_ms":312}
```

(`budget_exceeded` is only present, as `true`, when that call's cost exceeded `max_usd_per_call` — it's omitted, not written as `false`, in the common case shown above.)

For tools that ask more than one named SystemOne question per call
(`jev_verify`'s claims, `jev_check`'s propositions, `jev_classify`'s items,
`jev_rerank`'s candidates, `jev_review`/`jev_gate`'s five rubrics/claims,
`jev_compare`'s aspects, `jev_screen`'s up-to-three signals, `jev_ask`'s
questions, and `jev_decide`'s requirement checks), the audit entry's
`item_count`/`invalid_count` fields report how many named questions that one
call carried and how many came back `invalid_response`. `status` follows one
consistent rule across every tool in this codebase:

- `"ok"` — the call succeeded and every question's answer was well-formed
  (`invalid_count == 0`).
- `"invalid_response"` — the call succeeded, but at least one question's
  answer was malformed or missing (`invalid_count > 0`); the well-formed
  ones are still trustworthy, only the tool's own *output* distinguishes
  which is which.
- `"error"` — the call itself failed: a network error, a non-retryable or
  retry-exhausted HTTP error from OpenRouter, or a refused pre-call
  session-budget check. No model response was obtained at all.

Log write failures are printed to stderr and otherwise ignored — they never
fail the tool call itself.

## Budget enforcement

`max_usd_per_session` is enforced **before** sending a call, for every
tool: the server tracks exact cumulative spend (from OpenRouter's own
reported `usage.cost`) across every completed call this process has made,
and refuses a new call outright once that running total already meets or
exceeds the cap. (`jev_doctor` is the one exception to "refusal is a tool
error": it reports the refusal as `reachable: false` instead — see
[`jev_doctor`](tool-reference.md#the-jev_doctor-tool).)

`max_usd_per_call` can only be enforced **after** the fact: OpenRouter only
reports a call's actual cost in its response (see
`internal/openrouter/types.go`'s package doc comment), so this
implementation cannot know a specific call's cost before sending it without
guessing at both token count and pricing — which was not something we could
verify, so we didn't build it. When a completed call's actual cost exceeds
`max_usd_per_call`, the result is flagged (`budget_exceeded: true`) rather
than discarded: the model's answer(s) are still valid, and withholding an
already-paid-for judgment wouldn't protect the budget, since the spend
already happened. See the doc comments on `budget.Tracker` and
`internal/tools/score.ScoreOutput.BudgetExceeded` for the full reasoning.
