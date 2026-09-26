# Contributing to jev-mcp

This is the contributor entry point: how to set up, how a change moves from
idea to merge, and where to find things. Each section below points at the
full document in [`docs/`](docs/README.md).

## Set up

```sh
git clone <repo-url> && cd jev-mcp
go build -o jev-mcp .
```

Go 1.25+ and an OpenRouter API key are all you need — see
[Configuration](docs/configuration.md) for how the key is resolved. No
external services or containers are required: every test runs against an
in-process fake OpenRouter server (see
[Testing](docs/development.md#testing)).

## Make a change

### File an issue

Open an issue for a bug, a feature request, or a question, with the
acceptance criteria that define "done." Trivial changes (typos, docs) may
skip the issue and go straight to a pull request.

### Write the change

- Format with `gofmt` (`gofmt -l .` must report nothing) and keep
  `go vet ./...` clean.
- Every tool package follows the same self-registering shape — see
  [Architecture](docs/architecture.md#plugin-architecture) before adding or
  removing one.
- If your change alters documented behavior — a default, a threshold, a
  config field, a tool's input/output shape — update the matching section of
  [`docs/`](docs/README.md) in the same change. Most tool behavior lives in
  [Tool reference](docs/tool-reference.md).
- Run `go build ./...`, `go vet ./...`, `gofmt -l .`, and
  `go test -race -count=1 ./...` (see [Development](docs/development.md))
  before opening a pull request.

### Open a pull request

A pull request is a mechanism for review, not the historical record —
anything that must outlive the review belongs in the commit message. One
pull request may bundle several commits and close several issues; the
mapping is not required to be one-to-one.

## Where to find things

[`docs/README.md`](docs/README.md) indexes every document with a summary of
what it covers. [Architecture](docs/architecture.md) and
[Tool reference](docs/tool-reference.md) are the two you'll reach for most
while changing code.
