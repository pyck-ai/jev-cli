# Configuration

Where jev-cli gets its OpenRouter API key, the optional config file and the
environment variables that override it, the audit log every tool call
writes, and how per-call/session budget caps are enforced. See
[Development](development.md) for build/test/run commands, and
[Architecture](architecture.md) for how this plumbing is wired into every
tool.

## Route: LiteLLM proxy or direct OpenRouter

jev-cli sends SystemOne calls either through a local LiteLLM proxy (which
forwards `{base}/openrouter/api/v1/*` to `https://openrouter.ai/api/v1/*`
unchanged and swaps in its own OpenRouter key) or straight to OpenRouter.
The request body and headers are identical on both; only the URL and bearer
credential differ. `usage.cost` comes back unchanged, so budget caps work on
both. Implemented in `internal/route`; the proxy is preferred when present.

| Step | Rule |
|---|---|
| Candidate | `PYCKLLM_API_KEY` (LiteLLM virtual key) set, else config `proxy_api_key`. None: direct, no probe. |
| Base | `PYCKLLM_BASE_URL`, else config `proxy_base_url`, else `http://127.0.0.1:53986`. A trailing `/` and `/v1` are stripped. |
| Probe | One per process: `GET {base}/openrouter/api/v1/key` with the virtual key, 1.5 s timeout. `200` selects the proxy; anything else (refused, timeout, 401/403/404, ...) selects direct. |
| Direct | The OpenRouter key resolution below, unchanged. With no direct key either, startup fails with an error naming both routes. |
| Mid-session | Connection-level proxy failure (refused/timeout, no response) retries that call once direct, if a direct key exists, and marks the proxy down for the rest of the process. HTTP error responses from the proxy are real answers and are not retried direct. |

| Variable | Effect |
|---|---|
| `JEV_CLI_ROUTE` | `auto` (default), `proxy` (fail if the proxy is unusable, no fallback) or `direct` (never probe or use the proxy). No legacy `JEV_MCP_*` spelling. |
| `PYCKLLM_API_KEY` | LiteLLM virtual key. Env wins over config `proxy_api_key`. |
| `PYCKLLM_BASE_URL` | Proxy base root. Env wins over config `proxy_base_url`. |

`jev doctor` / `jev_doctor` report `route`, `route_why` (probe result),
`base_url` and `credential_source` (`env PYCKLLM_API_KEY`, `config
proxy_api_key`, `env OPENROUTER_API_KEY`, `opencode auth store at <path>`),
never a key. Startup logs `jev: route=... credential from ...` on stderr.
The virtual key is a revocable proxy credential, so it is the one key
allowed in the config file; the direct OpenRouter key never is.

## API key (direct route)

On the direct route, jev-cli resolves the OpenRouter API key it uses at
startup, in this order:

1. **`OPENROUTER_API_KEY` environment variable**, if set and non-empty:

   ```sh
   export OPENROUTER_API_KEY=sk-or-...
   ```

2. **Otherwise, opencode's own stored credentials.** If you already use
   [opencode](https://opencode.ai) and have logged into OpenRouter there,
   jev-cli will reuse that key instead of requiring a separate one. It reads
   (never writes, never requires opencode to be running) opencode's static
   credential file at `$XDG_DATA_HOME/opencode/auth.json`, falling back to
   `~/.local/share/opencode/auth.json` when `XDG_DATA_HOME` is unset. Only an
   `"openrouter"` entry with `"type": "api"` is usable this way:
   ```json
   { "openrouter": { "type": "api", "key": "sk-or-..." } }
   ```
   An oauth-based opencode login for OpenRouter, if that's ever a thing
   (`"type": "oauth"`, like the `anthropic` entry in opencode's own example),
   is **not** usable this way and is skipped — jev-cli only ever sends a
   plain `Authorization: Bearer <key>` header, it doesn't implement OAuth. A
   missing, unreadable, or malformed `auth.json` (bad JSON, wrong shape,
   permission denied) is treated the same as "no key here" and simply falls
   through, never a crash.

If neither source yields a key and no proxy is usable, the server fails fast at startup with a
clear message on stderr (see [Running standalone](development.md#running-standalone) for
what that looks like) — it never silently starts with no auth. Regardless
of which source is used, the resolved key is never read from jev-cli's own
config file below, never logged, and never included in an error message; a
startup log line on stderr does say *which source* was used (e.g. `using
OpenRouter key from env` or `using OpenRouter key from opencode auth store
at <path>`), for debugging, but never the key value itself. `jev_doctor`
also reports this same source string, per-call, as part of its
configuration snapshot (see [`jev_doctor`](tool-reference.md#the-jev_doctor-tool)).

## Config file (optional)

Path: `~/.config/jev-cli/config.json` (more precisely: `$XDG_CONFIG_HOME` or
`~/.config` if unset, joined with `jev-cli/config.json` — this is Go's
standard `os.UserConfigDir()`). Override the path with `JEV_CLI_CONFIG_PATH`.

**Rename fallback.** This project was previously called jev-mcp. If
`~/.config/jev-cli/config.json` doesn't exist but the old
`~/.config/jev-mcp/config.json` does, the old file is used. The old
environment variable names (`JEV_MCP_CONFIG_PATH`, `JEV_MCP_MODEL`) are
still honored too; when both old and new names are set, the new one wins.
The audit log (below) falls back to its old `jev-mcp` location the same
way. To finish migrating, move the old directories to their new names.
The file is entirely optional; if missing, the defaults below are used
as-is. If present, any field you omit keeps its default value.

```json
{
  "default_model": "typesafe/jev-latest",
  "tool_model_overrides": {},
  "budget": { "max_usd_per_call": 0.01, "max_usd_per_session": 1.0 },
  "retry": { "max_attempts": 3, "base_backoff_ms": 500, "max_backoff_ms": 8000 },
  "request_timeout_ms": 30000
}
```

| Field | Meaning |
|---|---|
| `default_model` | OpenRouter model slug used when a tool has no entry in `tool_model_overrides`. Built-in default `~typesafe/jev-latest`. See [Models](#models). |
| `tool_model_overrides` | Per-tool model slug overrides, keyed by tool name (any of the 14 tool names, e.g. `jev_review`, `jev_gate`, ...). Default empty (no tool is pinned). Example: `{"jev_review": "typesafe/jev-1.13"}`. |
| `budget.max_usd_per_call` | Per-call cost cap in USD. `<= 0` means unlimited. See [Budget enforcement](#budget-enforcement). |
| `budget.max_usd_per_session` | Cumulative cost cap in USD for this server process's lifetime. `<= 0` means unlimited. |
| `retry.max_attempts` | Total HTTP attempts per tool call (initial attempt + retries). |
| `retry.base_backoff_ms` / `retry.max_backoff_ms` | Jittered exponential backoff bounds between retries. |
| `request_timeout_ms` | Total deadline for a tool call, covering **all** retry attempts combined, not per-attempt. |
| `proxy_api_key` / `proxy_base_url` | Optional fallbacks for `PYCKLLM_API_KEY` / `PYCKLLM_BASE_URL` (env wins). Default none / `http://127.0.0.1:53986`. |

### Environment overrides

| Variable | Effect |
|---|---|
| `JEV_CLI_MODEL` | Overrides `default_model`. A tool-specific entry in `tool_model_overrides` still wins over this for that tool; the `--model` flag beats both. See [Models](#models). |
| `JEV_CLI_CONFIG_PATH` | Overrides the config file path. |
| `JEV_CLI_ROUTE`, `PYCKLLM_API_KEY`, `PYCKLLM_BASE_URL` | Route selection; see [Route](#route-litellm-proxy-or-direct-openrouter). |

## Models

jev-cli talks to OpenRouter's SystemOne decision models. No model list is
hard-coded: slugs, context lengths and prices come from the API
(`GET <apiBase>/models?output_modalities=decisions`, direct or through the
proxy, per the active [route](#route-litellm-proxy-or-direct-openrouter)).

### Selecting a model

Highest precedence first (`internal/config`: `ForceModel`, `ModelForTool`,
`applyEnvOverrides`):

| # | Source | Scope |
|---|---|---|
| 1 | `--model <slug>` (persistent root flag, any subcommand incl. `mcp`) | Forces every tool; drops `tool_model_overrides`. |
| 2 | `tool_model_overrides[tool]` in the config file | That tool only. |
| 3 | `JEV_CLI_MODEL` | Replaces `default_model` for every tool without an override. |
| 4 | `default_model` in the config file | Every tool without an override. |
| 5 | Built-in `~typesafe/jev-latest` | Fallback. |

### `jev models`

`jev models [--refresh] [-o text|json]` lists the catalog: slug (aliases
shown as `alias -> target`), context length, input price per 1M tokens,
input modalities, creation date. `-o json` emits
`{fetched_at, source, models[]}`; `source` is `cache`, `network` or
`stale-cache`. Needs a usable route/credential like any tool call.

| Item | Behavior |
|---|---|
| Catalog cache | `$XDG_CACHE_HOME/jev-cli/models.json` (`os.UserCacheDir`, i.e. `$HOME/.cache/jev-cli/` if unset; `/tmp/.cache/jev-cli/` in the Docker image). Written atomically. |
| TTL | 24h. A fresh cache is used with no network call. |
| Fetch fails | Any cache, even expired, is used with a stderr warning. No cache at all: error. A corrupt or unwritable cache is never fatal. |
| `--refresh` | Refetches regardless of age and deletes the learned limits (below). |

### Pre-send guard

Every call is checked by a client hook (`internal/models/guard.go`, see
[Architecture](architecture.md#shared-plumbing)) before anything is sent.
A refusal is a `PreflightError`, surfaced as a normal tool error; nothing
is billed.

| Check | Behavior |
|---|---|
| Context length | Input tokens are estimated as bytes/4 (state JSON plus every question's instructions and criteria). Over the model's catalog `context_length`: refused. `context_length` 0 (unknown): no check. |
| Unknown model | Slug not in the catalog: one stderr warning per slug, request still sent (the provider's answer is authoritative). |
| Catalog unavailable | One stderr warning, no checks, request sent. |
| Learned limits | An HTTP 400 for a catalog model is stored in `limits.json` next to `models.json` for 24h, keyed by request **shape** only (question types, state kind and top-level keys, max choice-option count, log2 size bucket of the state, question count); never the content. An equivalent request is then refused early, quoting the provider's message. "Model does not exist" 400s are not learned. `jev models --refresh` clears them. |

### Model compatibility (observed)

Observation from live calls on 2026-10-06 through the PYCKLLM proxy, not a
promise; `jev models` is the live source. Models listed by OpenRouter at
that time (13) were probed with one `noul`, one `choice` and one `score`
question:

| Result | Models |
|---|---|
| Answered all three, no code changes | `~typesafe/jev-latest`, `typesafe/jev-1.13`, `liquid/d1`, `cloudflare/clef`, `cloudflare/clef-flash`, `upstage/solar-decide`, `inception/mercury-decide:free`, `jaredpalmer/kev-4b`, `perplexity/pplx-decider-v1-27b`, `togethercomputer/tev1-4b-experimental` |
| `noul` only | `respan/span-01*`: a `choice` question gets HTTP 400 (only `noul` questions with plain-string instructions and criteria are accepted). Tools that ask other question types fail with that error. |

Provider limits differ (for example `jaredpalmer/kev-4b` answers an
oversized input with an opaque 400); the guard learns those on first
failure.

## Audit log

Every tool call appends one JSON line to
`$XDG_DATA_HOME/jev-cli/audit.jsonl` (falling back to
`~/.local/share/jev-cli/audit.jsonl` when `XDG_DATA_HOME` is unset). Parent
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
