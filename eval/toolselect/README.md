# eval/toolselect

Offline eval: which jev MCP tool does an LLM agent pick for a task? Use it to measure the effect of edits to tool descriptions. It sends one single-turn chat completion per trial, scores the first tool call, and never invokes a jev handler. Not shipped in the image (`.dockerignore` whitelists only `go.mod`, `go.sum`, `cmd/`, `internal/`).

## Run

```sh
export PYCKLLM_API_KEY=...                       # LiteLLM proxy key
task eval:toolselect                             # all cases, haiku
task eval:toolselect -- --models '~anthropic/claude-haiku-latest,z-ai/glm-5.3-flash' --reps 3
go run ./eval/toolselect --filter decide --models openai/gpt-5-mini
go run ./eval/toolselect --openrouter --models anthropic/claude-haiku-4.5   # direct, needs OPENROUTER_API_KEY
go run ./eval/toolselect --show-tools | jq '.[].name'                       # tools exactly as the agent sees them, no network
```

| Flag | Default | Meaning |
|---|---|---|
| `--models` | `~anthropic/claude-haiku-latest` | comma list of agent model ids |
| `--cases` | `eval/toolselect/cases.json` | case file (run from the repo root) |
| `--filter` | | only case ids containing this substring |
| `--reps` | 1 | repetitions per case and model |
| `--concurrency` | 4 | parallel requests |
| `--timeout` | 90s | per request; 429/5xx are retried twice |
| `--prefix` | `jev_` | host-added prefix; opencode shows `jev_jev_ask` |
| `--max-tokens` | 4096 | headroom for reasoning models |
| `--base-url`, `--api-key-env` | `$PYCKLLM_BASE_URL` or `http://127.0.0.1:53986`, `PYCKLLM_API_KEY` | endpoint and key env var |
| `--openrouter` | off | base `https://openrouter.ai/api/v1`, key `OPENROUTER_API_KEY`, every model on `/chat/completions` |
| `--out` | `/tmp/opencode/jev-eval/runs/<timestamp>.jsonl` | one JSON line per trial; never in the repo |
| `--json` | off | machine-readable report on stdout |
| `--yes`, `--max-usd` | off, 1.0 | run is refused above the estimate unless `--yes` |

Routing on the proxy: ids starting `anthropic/` or `~anthropic/` go to `<base>/v1/chat/completions`, everything else to `<base>/openrouter/api/v1/chat/completions`. Requests use `tool_choice: "auto"` (an agent may legitimately answer in text) and `temperature: 0`, dropped automatically for models that reject it.

## Tool list fidelity

`tools.go` registers every tool from `internal/registry` on an in-process MCP server (zero-value `registry.Deps`; handlers are never called), connects an in-memory client, calls `tools/list`, and converts each tool to an OpenAI function: `name = prefix + name`, `parameters = inputSchema`. Descriptions are always the current code. Like `cmd/jev/main.go`, `main()` sets `JSONSCHEMAGODEBUG=typeschemasnull=1` before any schema inference, so slices are plain `"type":"array"` exactly as MCP clients of the real binary see them (a test guards this). Add a tool package and blank-import it in `tools.go` (same as `cmd/jev/main.go`); the unit test fails if a registered tool is missing from the list.

## Metrics

Per trial verdict, first match wins:

| Verdict | Meaning |
|---|---|
| `ideal` | chosen == `ideal` |
| `none` | no tool call (the agent answered in text; the text is kept in the JSONL) |
| `acceptable` | chosen is in `acceptable` |
| `escape` | chose `ask` although `ask` is not acceptable |
| `wrong` | any other tool |
| `truncated` | no tool call and `finish_reason == "length"` (max_tokens hit before the model acted); excluded from the ideal/accept/escape/none denominators |
| `error` | request failed after retries; excluded from rates |

Report columns: `trials` (scored, errors excluded), `n_eff` (`n_effective`: trials minus truncated, the denominator of the rates below), `errors`, `trunc%` (truncated / trials), `ideal%`, `accept%` (ideal + acceptable), `escape%`, `none%` (all over `n_effective`), `schema%` (of trials with a tool call: required keys present and top-level types right, a deliberately simple check), mean latency, reported cost. Below the table: each non-acceptable pick as case, ideal, chosen, count. A control case with `ideal: "none"` expects no tool call.

## Cases

`cases.json` holds `system_prompt` (versioned with the cases) and `cases[]` with `id`, `category`, `prompt`, `ideal` (bare tool name, e.g. `decide`, or `none`), `acceptable` (list), `notes` (why this label). To add one: append an object, keep the id unique, run `go test ./eval/...` (checks tool names, duplicates, prompts naming a tool, and that every tool is the ideal pick of at least two cases). Prompts must not name a tool. Set `acceptable` where a second tool is genuinely defensible (batched independent decisions, mixed rubrics), not to forgive misroutes.

**Privacy: cases are synthetic only.** The repo is public. Never copy text, names or domain details from real sessions, traces or tickets.

## Cost

Estimate before the run: roughly (tool list tokens + system prompt + case prompt) in plus about 400 tokens out, per trial, times cases x models x reps, priced from a small built-in table. The tool list alone is ~9k input tokens per trial, so cost is dominated by input. The estimate is printed to stderr; above `--max-usd` (default $1) the run is refused without `--yes`. Reported `usage.cost` is summed in the table when the endpoint returns it (the proxy does for OpenRouter models, not for Anthropic).
