# syntax=docker/dockerfile:1

# Both refs come from buildargs.conf (mirrored as defaults in docker-bake.hcl;
# the defaults here only keep a bare `docker build .` working and satisfy the
# build linter). They are floating tags on purpose: freshness comes from the
# scheduled rebuild with --pull, not from pins.
ARG GOLANG_IMAGE=ghcr.io/pyck-ai/baseimages/golang:alpine
ARG RUNTIME_IMAGE=ghcr.io/pyck-ai/baseimages/static:latest

#===============================================================================
# BUILD
#===============================================================================

# Runs on the builder's own platform and cross-compiles, so the arm64 image
# never emulates the Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM ${GOLANG_IMAGE} AS build
ARG TARGETOS TARGETARCH

WORKDIR /src

# Dependencies first so source-only changes reuse this layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,id=go-mod,target=/go/pkg/mod,sharing=shared \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,id=go-mod,target=/go/pkg/mod,sharing=shared \
    --mount=type=cache,id=go-build,target=/var/cache/go,sharing=shared \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /out/jev ./cmd/jev

#===============================================================================
# RESULT
#===============================================================================

# FROM scratch with CA certificates, a numeric nonroot uid (1001) and a
# world-writable /tmp. No shell.
FROM ${RUNTIME_IMAGE}

COPY --from=build /out/jev /usr/local/bin/jev

# jev resolves its config dir and audit log path from $HOME unconditionally at
# startup, and exits 3 when it cannot. The base image's HOME
# (/home/nonroot) does not exist for an arbitrary `--user uid:gid`, whereas
# /tmp is 1777 and works for any uid.
ENV HOME=/tmp

ENTRYPOINT ["/usr/local/bin/jev"]
