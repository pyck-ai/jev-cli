# Docker Bake configuration for the jev-cli image.
#
# Build args are read from buildargs.conf via environment variables. Task
# sources that file before calling bake. The defaults below mirror it so a bare
# `docker buildx bake` works too.
#
# Usage:
#   task build
#
# Or directly with docker:
#   set -a && source buildargs.conf && set +a && docker buildx bake

variable "REGISTRY" {
  default = "ghcr.io/pyck-ai"
}

variable "GOLANG_IMAGE" {
  default = "ghcr.io/pyck-ai/baseimages/golang:alpine"
}

variable "RUNTIME_IMAGE" {
  default = "ghcr.io/pyck-ai/baseimages/static:latest"
}

group "default" {
  targets = ["jev"]
}

target "jev" {
  context    = "."
  dockerfile = "Dockerfile"
  args = {
    GOLANG_IMAGE  = GOLANG_IMAGE
    RUNTIME_IMAGE = RUNTIME_IMAGE
  }
  platforms = [
    "linux/amd64",
    "linux/arm64",
  ]
  # One tag only: jev-cli has no releases, so there is no version ladder and
  # no per-commit tag. Consumers pin latest@sha256:...
  tags = ["${REGISTRY}/jev-cli:latest"]
  labels = {
    "org.opencontainers.image.source"      = "https://github.com/pyck-ai/jev-cli"
    "org.opencontainers.image.title"       = "jev-cli"
    "org.opencontainers.image.description" = "jev CLI and MCP server for the typesafe/jev model family via OpenRouter"
    "org.opencontainers.image.licenses"    = "MIT"
  }
  cache-from = ["type=registry,ref=${REGISTRY}/jev-cli/buildcache:jev"]
  cache-to   = ["type=registry,ref=${REGISTRY}/jev-cli/buildcache:jev,mode=max"]
}
