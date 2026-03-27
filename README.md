# ArtifactFS

> This is a beta release of ArtifactFS. Your mileage may vary.

ArtifactFS is a Git-backed filesystem daemon (FUSE driver) in Go that mounts repositories as normal working trees while avoiding eager blob downloads.

It is designed to allow fast clones that can hydrate and stream in files on-the-fly, enabling operations over the repo without blocking on the entire clone. This is especially advantageous for agents, sandboxes and ad-hoc operations over git repositories where "cold start" performance is critical.

Notably:

* The operating system sees the entire file-tree almost immediately, while the underlying FUSE driver downloads contents. It prioritizes package and dependency manifests and code over binary blobs, and is designed to reduce blocking time for agents working on repositories.
* This project is designed as part of [Cloudflare Artifacts](http://workers.cloudflare.com/product/artifacts), a massively scalable, versioned filesystem that supports the git protocol, as well as native TypeScript SDKs and REST APIs, but works with any git repo.
* It is optional: Artifacts can act as git repos and be cloned directly, without needing ArtifactFS. But larger repositories (e.g. 500MB+) and/or repositories with millions of objects can take some time to clone, which blocks agents until the clone completes. ArtifactFS allows you to mount the repo directly, loading blob (file) contents in the background.

## What are Cloudflare Artifacts?

TODO - describe briefly and link to [Cloudflare Artifacts](http://workers.cloudflare.com/product/artifacts)

## Build and Install

Requires Go 1.24+ and [macFUSE](https://osxfuse.github.io/) on macOS.

```bash
go build -o artifact-fs ./cmd/artifact-fs
```

Quick start against a public repo:

```bash
export ARTIFACT_FS_ROOT=/tmp/artifact-fs-test

# Register and clone (returns immediately)
./artifact-fs add-repo \
  --name workers-sdk \
  --remote https://github.com/cloudflare/workers-sdk.git \
  --branch main \
  --mount-root /tmp

# Start the daemon (mounts via FUSE, blocks until killed)
./artifact-fs daemon --root /tmp &
DAEMON_PID=$!

# Use the repo
ls /tmp/workers-sdk/
cat /tmp/workers-sdk/README.md
git -C /tmp/workers-sdk log --oneline -5

# Cleanup
kill $DAEMON_PID
```

## Sandboxes and Containers

[`examples/Dockerfile`](examples/Dockerfile) builds artifact-fs and starts a FUSE-mounted repo inside a container. The container requires `--cap-add SYS_ADMIN --device /dev/fuse` for FUSE access.

```bash
# Build the image
docker build -t artifact-fs-example -f examples/Dockerfile .

# Run with the default repo (cloudflare/workers-sdk)
docker run --rm --cap-add SYS_ADMIN --device /dev/fuse artifact-fs-example

# Run with a private repo
docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
  -e REPO_REMOTE_URL=https://<token>@github.com/org/private-repo.git \
  artifact-fs-example

# Run a command inside the mounted repo
docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
  artifact-fs-example git log --oneline -5
```

The entrypoint registers the repo, starts the daemon, waits for the mount, then either runs the provided command or keeps the container alive.

On hosts with AppArmor enabled (Ubuntu default), add `--security-opt apparmor:unconfined` to the `docker run` flags.

## Architecture

This implementation includes:

- Registry-backed repo lifecycle (`add-repo`, `remove-repo`, `list-repos`, `status`, `fetch`, `remount`, `unmount`, `set-refresh`)
- Blobless clone and Git substrate via system `git`
- Snapshot index persisted in SQLite with generation publishing
- Persistent writable overlay (`upper` + SQLite metadata + whiteouts)
- Hydrator queue with per-object deduped waiters and priority classification
- Merged view resolver and operation engine (`Lookup`, `Getattr`, `Readdir`, read/write path semantics)
- Token redaction utilities for logs and command output

## Supported git operations

Work in progress. The table below reflects operations tested against [cloudflare/workers-sdk](https://github.com/cloudflare/workers-sdk) mounted via macFUSE.

### Filesystem operations

| Operation | Status | Notes |
|-----------|--------|-------|
| `ls` (root and subdirectories) | Supported | Includes synthesized `.git` gitfile |
| `cat` / read file | Supported | Triggers on-demand hydration for unhydrated blobs |
| `stat` (file size, mode) | Supported | Sizes resolved via `git cat-file --batch-check` |
| `mkdir` | Supported | Persisted in writable overlay |
| Create new file | Supported | Persisted in writable overlay |
| Write / append to file | Supported | Copy-on-write for tracked files |
| Rename file | Supported | Works for both overlay and tracked (snapshot-only) files |
| Delete file (`rm`) | Supported | Whiteout recorded in overlay |
| `rmdir` | Supported | Checks directory is empty first |
| Truncate | Supported | Hydrates blob before truncating |
| Symlink read (`readlink`) | Supported | Symlink target read from blob content |

### Git operations

| Operation | Status | Notes |
|-----------|--------|-------|
| `git log` | Supported | Reads from pack objects |
| `git branch` | Supported | |
| `git rev-parse HEAD` | Supported | |
| `git show` | Supported | |
| `git remote -v` | Supported | Credentials stripped from output |
| `git stash list` | Supported | |
| `git status` | Supported | ~7s on 5800-entry repo; some unicode-named files show as deleted |
| `git diff` | Supported | Shows correct unified diff for modified files |
| `git add` | Supported | Stages modified files |
| `git reset` | Supported | ~6.5s index refresh |
| `git fetch` | Supported | Background refresh loop fetches periodically |

### Known limitations

| Issue | Impact |
|-------|--------|
| Files with non-ASCII names (e.g. `♫`, `ü`) may show as deleted in `git status` | Low -- path encoding mismatch between git index and FUSE tree |
| `mtime` is always `time.Now()`, not the actual commit timestamp | Cosmetic |
| `git commit` is untested | Likely works but not yet verified |
| Branch switching via `git checkout` not yet wired | Watcher detects HEAD changes but overlay reconciliation is a v1 stub |

## Testing

Unit tests:

```bash
go test ./...
```

End-to-end tests mount a real git repo via macFUSE and exercise filesystem + git operations with a real git client. They require macFUSE to be installed and are off by default.

```bash
# Run e2e tests against the default repo (cloudflare/workers-sdk)
AFS_RUN_E2E_TESTS=1 go test -v -run TestE2E -count=1 -timeout 10m .

# Run against a private repo with authentication
AFS_RUN_E2E_TESTS=1 \
  AFS_E2E_REPO=https://ghp_yourtoken@github.com/org/private-repo.git \
  go test -v -run TestE2E -count=1 -timeout 10m .
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AFS_RUN_E2E_TESTS` | `0` | Set to `1` to enable end-to-end tests |
| `AFS_E2E_REPO` | `https://github.com/cloudflare/workers-sdk.git` | Git HTTPS remote URL for e2e tests. Accepts authenticated URLs to avoid rate limits or test against private repos. |
| `ARTIFACT_FS_ROOT` | `~/.local/share/artifact-fs` (macOS) or `/var/lib/artifact-fs` (Linux) | Runtime data root for the daemon and CLI |

## Contributing

TODO

## Credits

The ArtifactFS FUSE driver takes inspiration from and draws from implementation details in:

* [TigrisFS](https://github.com/tigrisdata/tigrisfs/)
* [gitfs](https://github.com/presslabs/gitfs)
* [SlothFS](https://gerrit.googlesource.com/gitfs/)

## License

(c) Cloudflare, 2026. Apache-2.0 licensed.
