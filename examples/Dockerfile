# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /artifact-fs ./cmd/artifact-fs

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    fuse3 git ca-certificates \
  && rm -rf /var/lib/apt/lists/*

COPY --from=build /artifact-fs /usr/local/bin/artifact-fs

ENV REPO_REMOTE_URL=https://github.com/cloudflare/workers-sdk.git
ENV REPO_BRANCH=main
ENV REPO_NAME=repo
ENV ARTIFACT_FS_ROOT=/var/lib/artifact-fs
ENV MOUNT_ROOT=/mnt

COPY --chmod=0755 examples/entrypoint.sh /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
