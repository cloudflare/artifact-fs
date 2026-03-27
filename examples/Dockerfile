# syntax=docker/dockerfile:1

# Builds artifact-fs and starts a FUSE-mounted git repo inside the container.
# The container needs --cap-add SYS_ADMIN --device /dev/fuse to use FUSE.
#
# Build:
#   docker build -t artifact-fs-example -f examples/Dockerfile .
#
# Run (public repo):
#   docker run --rm --cap-add SYS_ADMIN --device /dev/fuse artifact-fs-example
#
# Run (private repo):
#   docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
#     -e REPO_REMOTE_URL=https://<token>@github.com/org/private-repo.git \
#     artifact-fs-example
#
# On hosts with AppArmor (Ubuntu), add: --security-opt apparmor:unconfined

# ---- build stage ----
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /artifact-fs ./cmd/artifact-fs

# ---- runtime stage ----
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

COPY examples/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
