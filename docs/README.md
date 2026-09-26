# Documentation

This directory holds every detailed document for jev-mcp, one topic per
file. The entrypoints elsewhere in the repository are deliberately thin:
[`README.md`](../README.md) is the project map and
[`CONTRIBUTING.md`](../CONTRIBUTING.md) is the contributor guide; each
summarizes and links here rather than restating.

## Documents

**[Configuration](configuration.md)** — how jev-mcp gets its OpenRouter API
key (env var, then opencode's own credential store), the optional config
file and its environment-variable overrides, the JSON-lines audit log every
call writes, and how the per-call/session budget caps are enforced.

**[Architecture](architecture.md)** — how every tool is a self-registering
plugin, and how to add or remove one; the shared plumbing packages every
tool reuses; the conventions every tool follows (fail-closed statuses,
`auto_accept` thresholds, batching); and the repository's package layout.

**[Development](development.md)** — how to build the binary, run the test
suite (and why no `OPENROUTER_API_KEY` is needed to do so), sanity-check it
standalone over stdio, and register it with opencode.

**[Tool reference](tool-reference.md)** — the full input/output spec and an
example call/response for each of the 14 tools. Dense reference material,
meant to be searched rather than read front-to-back.

## Maintenance

This index must always match the files on disk. When you add, remove, or
rename a document in `docs/`, or change what one covers, update this README
**in the same change** — and fix the root [`README.md`](../README.md) or
[`CONTRIBUTING.md`](../CONTRIBUTING.md) too if the change affects what they
summarize.

Each fact lives in exactly one document. If you're moving a subject from one
document to another, move it in both entries above rather than leaving it in
two places.
