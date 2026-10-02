# Agent Instructions

This repository is a Go CLI and MCP server (`jev`, main package `./cmd/jev`) and also builds exactly one image, `ghcr.io/pyck-ai/jev-cli`, from [`Dockerfile`](Dockerfile). Contributor flow is in [`CONTRIBUTING.md`](CONTRIBUTING.md); architecture and development docs are indexed in [`docs/README.md`](docs/README.md).

## Image

- One bake target (`jev`) in [`docker-bake.hcl`](docker-bake.hcl), one tag (`latest`), `linux/amd64` + `linux/arm64`. No semver ladder: jev-cli has no releases. Consumers pin `latest@sha256:...`.
- Base images (`GOLANG_IMAGE`, `RUNTIME_IMAGE`) are floating tags in [`buildargs.conf`](buildargs.conf), on purpose: a scheduled rebuild picks up fresh bases because the shared workflow builds with `--pull`. Do not pin digests there and do not add Renovate config for them. `buildargs.conf` must keep at least one non-comment `KEY=VALUE` line: the shared workflow fails on an empty one.
- The build stage runs on `$BUILDPLATFORM` and cross-compiles (`GOOS`/`GOARCH` from `TARGETOS`/`TARGETARCH`); never switch it to emulated builds.
- [`.dockerignore`](.dockerignore) is a whitelist (`go.mod`, `go.sum`, `cmd/`, `internal/`). A new top-level directory the build needs must be re-included there and copied in the `Dockerfile`.
- Triggers in [`build-image.yml`](.github/workflows/build-image.yml): weekday schedule, `workflow_dispatch`, `pull_request`, and `push` to `main` (unlike the other image repos, because the image content is this repo's code). [`tidy-ghcr.yml`](.github/workflows/tidy-ghcr.yml) and [`.ghcr-tidy.yaml`](.ghcr-tidy.yaml) handle GHCR retention. Cron slots are staggered across the pyck-ai image repos; check the others before changing one.

## Default user and HOME

The runtime is `FROM scratch` (no shell) and defaults to **uid/gid 1001**, the uid our GitHub Actions runners execute as. `ENV HOME=/tmp` is load-bearing: `jev` resolves its config and audit log paths from `$HOME` at startup (`cmd/jev/main.go`, `internal/config`, `internal/xdg`, `internal/audit`) and exits 3 without one, including for an arbitrary `--user uid:gid`, which has no home directory. `/tmp` is world-writable (1777). Do not drop it.

## Verification

CI runs [`verify.sh`](verify.sh) **inside** the exact pushed digest before `latest` is applied: `docker run --rm --env-file buildargs.conf -e TARGET=jev -v "$(pwd)/verify.sh:/verify.sh:ro" --entrypoint /bin/sh <ref> /verify.sh`. The image has no shell, so the verify-image action overlays busybox's `/bin` first; reproduce that by hand with `printf 'FROM <ref>\nCOPY --from=busybox:musl /bin /bin\n' | docker build -t jev-cli-verify -`. It needs no network and no real key.

When the `Dockerfile` changes, update `verify.sh` in the same change: default user, `HOME`, shipped files, and the startup check (`jev check` with no key must exit 3 naming OpenRouter).

## Docs

If a change alters the image, its tag, user, `HOME` or build files, update the "Docker image" sections of [`README.md`](README.md) and [`docs/development.md`](docs/development.md) in the same change.
