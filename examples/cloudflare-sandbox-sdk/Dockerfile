# syntax=docker/dockerfile:1

# Build ArtifactFS from this repository, then add it to the stock Sandbox image.
# Keep the base image's default startup flow intact so the Sandbox SDK still works.
FROM golang:1.26-bookworm AS artifact-fs-build

ARG ARTIFACT_FS_EXTRA_CA_PEM=""

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
  && update-ca-certificates \
  && if [ -n "$ARTIFACT_FS_EXTRA_CA_PEM" ]; then \
       printf '%s\n' "$ARTIFACT_FS_EXTRA_CA_PEM" >/usr/local/share/ca-certificates/artifact-fs-extra-ca.crt; \
       update-ca-certificates; \
     fi \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /artifact-fs ./cmd/artifact-fs

FROM docker.io/cloudflare/sandbox:0.12.5

ARG ARTIFACT_FS_EXTRA_CA_PEM=""

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    fuse3 \
    git \
    openssh-client \
    util-linux \
  && if [ -n "$ARTIFACT_FS_EXTRA_CA_PEM" ]; then \
       printf '%s\n' "$ARTIFACT_FS_EXTRA_CA_PEM" >/usr/local/share/ca-certificates/artifact-fs-extra-ca.crt; \
       update-ca-certificates; \
     fi \
  && rm -rf /var/lib/apt/lists/*

COPY --from=artifact-fs-build /artifact-fs /usr/local/bin/artifact-fs
COPY --chmod=0755 examples/cloudflare-sandbox-sdk/container_src/mount-artifact-fs-repo.sh /usr/local/bin/mount-artifact-fs-repo

# Public repo that works out of the box. The Worker can override these per request.
ENV MOUNT_GIT_REMOTE=https://github.com/cloudflare/sandbox-sdk.git
ENV MOUNT_GIT_BRANCH=main
ENV ARTIFACT_FS_ROOT=/tmp/artifact-fs
ENV MOUNT_ROOT=/workspace/mnt
ENV ARTIFACT_FS_MOUNT_METADATA_FILE=/workspace/.artifact-fs-mount
