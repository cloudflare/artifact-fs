<p align="center">
  <img src="artifacts.png" alt="Artifacts" width="720">
</p>

# ArtifactFS

[![Build & Test](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml/badge.svg)](https://github.com/cloudflare/artifact-fs/actions/workflows/build-test.yml)

> This is a beta release of ArtifactFS. Your mileage may vary.

ArtifactFS is a Git-backed filesystem daemon (FUSE driver) in Go that mounts repositories as normal working trees while avoiding eager blob downloads.

**It is designed to allow fast clones that can hydrate and stream in files on-the-fly, enabling operations over the repo without blocking on the entire clone. This is especially advantageous for agents, sandboxes and ad-hoc operations over git repositories where "cold start" performance is critical.**

Notably:

* The operating system sees the entire file-tree almost immediately, while the underlying FUSE driver downloads contents. It prioritizes package and dependency manifests and code over binary blobs, and is designed to reduce blocking time for agents working on repositories.
* This project is designed as part of [Cloudflare Artifacts](http://workers.cloudflare.com/product/artifacts), a massively scalable, versioned filesystem that supports the git protocol, as well as native TypeScript SDKs and REST APIs, but works with any git repo.
* It is optional: Artifacts can act as git repos and be cloned directly, without needing ArtifactFS. But larger repositories (e.g. 500MB+) and/or repositories with millions of objects can take some time to clone, which blocks agents until the clone completes. ArtifactFS allows you to mount the repo directly, loading blob (file) contents in the background.

## What are Cloudflare Artifacts?

[Cloudflare Artifacts](https://workers.cloudflare.com/product/artifacts) is a versioned filesystem that speaks git. Create a repo per user, per agent session, per sandbox... as many as you need. It's designed for agent toolchains, sandboxes, and CI/CD systems that need fast, scalable access to code repositories.

ArtifactFS is the optional FUSE driver -- it lets you mount an Artifact (or any git repo) as a local filesystem without waiting for a full clone.

## Build and Install

Requires Go 1.24+ and a FUSE implementation:

- **macOS** -- [macFUSE](https://osxfuse.github.io/)
- **Linux** -- `fuse3` (`apt install fuse3` on Debian/Ubuntu, `dnf install fuse3` on Fedora)

Install the CLI from the module:

```bash
go install github.com/cloudflare/artifact-fs/cmd/artifact-fs@latest
```

Or build it directly from the module path:

```bash
go build -o artifact-fs github.com/cloudflare/artifact-fs/cmd/artifact-fs
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

## Monitoring hydration and repo status

Check the state of a mounted repo with `status`:

```bash
./artifact-fs status --name workers-sdk
# repo=workers-sdk state=mounted head=d4c61587... ref=main ahead=0 behind=0 diverged=false last_fetch=2026-03-27T12:00:00Z result=ok overlay_dirty=false
```

| Field | Meaning |
|-------|---------|
| `state` | `mounted` or `unmounted` |
| `head` | Current HEAD commit OID |
| `ref` | Tracked branch |
| `ahead` / `behind` | Commits ahead/behind the remote tracking branch |
| `overlay_dirty` | `true` if there are local writes (created, modified, or deleted files) |
| `last_fetch` / `result` | Timestamp and outcome of the last background fetch |

Hydration (blob downloading) is transparent: the file tree is visible immediately after mount, and reads block only until the requested blob is fetched. The daemon prioritizes code and manifests (`package.json`, `go.mod`, `README.md`) over binary files.

To monitor hydration activity, watch the daemon's JSON log output:

```bash
./artifact-fs daemon --root /tmp 2>/tmp/daemon.log &
# In another terminal:
tail -f /tmp/daemon.log | grep -i hydrat
```

Use `--hydration-concurrency` to control the number of parallel blob-fetch workers (default 4). Each worker maintains a persistent `git cat-file --batch` process, so higher values trade memory for faster bulk hydration:

```bash
./artifact-fs daemon --root /tmp --hydration-concurrency 8
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

ArtifactFS has two distinct phases: a one-shot **setup** (`add-repo`) that performs a fast blobless clone of a repo, and a long-running **daemon** that mounts it via FUSE and serves file operations.

```
                         ┌─────────────────────────────────────────────────┐
                         │                    Daemon                       │
                         │                                                 │
  ┌──────────┐  clone    │  ┌──────────┐    ls-tree     ┌──────────────┐  │
  │  Remote  │◄──────────┼──│ GitStore │───────────────►│   Snapshot   │  │
  │   repo   │  fetch    │  │          │  cat-file       │   (SQLite)   │  │
  └──────────┘           │  │ batch    │  --batch-check  │              │  │
                         │  │ pool     │                 │  base_nodes  │  │
                         │  └────┬─────┘                 │  per gen     │  │
                         │       │ cat-file               └──────┬───────┘  │
                         │       │ --batch                       │          │
                         │       ▼                               ▼          │
                         │  ┌──────────┐                 ┌──────────────┐  │
                         │  │  Blob    │                 │   Resolver   │  │
                         │  │  Cache   │                 │              │  │
                         │  │  (disk)  │◄────hydrate─────│ snap + ovl   │  │
                         │  └──────────┘                 │  merged view │  │
                         │       ▲                       └──────┬───────┘  │
                         │       │                              │          │
                         │  ┌────┴─────┐   prefetch      ┌─────┴────────┐ │
                         │  │ Hydrator │◄────────────────│    Engine    │ │
                         │  │          │                  │              │ │
                         │  │ priority │   ensureOverlay  │ read / write │ │
                         │  │ queue    │   copy-on-write  │ create / rm  │ │
                         │  └──────────┘                  └─────┬────────┘ │
                         │                                      │          │
                         │  ┌──────────┐                 ┌──────┴───────┐  │
                         │  │ Overlay  │◄────────────────│  FUSE Layer  │  │
                         │  │ (SQLite  │  write ops      │  (macFUSE /  │  │
                         │  │  + upper │                  │   /dev/fuse) │  │
                         │  │  dir)    │                  └──────┬───────┘  │
                         │  └──────────┘                         │          │
                         │                                       │          │
                         │  ┌──────────┐  HEAD poll       ┌─────┴────────┐ │
                         │  │ Watcher  │─────────────────►│ Mount point  │ │
                         │  │ (500ms)  │  re-index +      │ /tmp/myrepo  │ │
                         │  └──────────┘  reconcile       └──────────────┘ │
                         └─────────────────────────────────────────────────┘
```

### Data flow

1. **Clone** -- `add-repo` runs `git clone --filter=blob:none` (blobless). Only commits, trees, and refs are fetched. No file content is downloaded.

2. **Index** -- `git ls-tree -r -t -z HEAD` enumerates every path in the tree. Sizes are resolved locally via `git cat-file --batch-check` with `GIT_NO_LAZY_FETCH=1` to avoid network round-trips. The result is bulk-inserted into a SQLite `base_nodes` table as a new generation.

3. **Mount** -- The FUSE layer exposes the tree immediately. A synthesized `.git` gitfile points at the real gitdir so git commands work inside the mount.

4. **Read** -- The Resolver merges the snapshot (base tree) with the overlay (local writes). For base files, reads block until the Hydrator fetches the blob via a persistent `git cat-file --batch` process and streams it to the blob cache.

5. **Write** -- The Engine promotes base files to the overlay via copy-on-write (hydrate, then copy to the `upper/` directory). Subsequent reads come from the overlay. Deletes are recorded as whiteouts.

6. **Background** -- A watcher polls HEAD/refs every 500ms. On HEAD changes (commit, branch switch, fetch), the daemon re-indexes the tree, publishes a new snapshot generation, reconciles stale overlay entries, and refreshes the git index.

### Subsystems

| Package | Role |
|---------|------|
| `daemon` | Orchestrates repo lifecycle, refresh loop, watcher callbacks |
| `fusefs` | FUSE adapter (inode management, op dispatch), Resolver (merged view), Engine (read/write logic) |
| `gitstore` | Git CLI wrapper: clone, fetch, ls-tree, batch pool for `cat-file --batch` |
| `snapshot` | SQLite store for `base_nodes` keyed by `(generation, path)` |
| `overlay` | SQLite metadata + `upper/` directory for local writes, whiteouts, reconciliation |
| `hydrator` | Priority queue with deduped waiters; workers block on a `workReady` channel |
| `watcher` | Polls gitdir mtimes (HEAD, index, refs) at 500ms intervals |
| `registry` | SQLite-backed repo config persistence |
| `model` | Shared types and canonical interfaces (`GitStore`, `SnapshotStore`, `OverlayStore`, `Hydrator`) |

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
| `git status` | Supported | ~7s on 5800-entry repo |
| `git diff` | Supported | Shows correct unified diff for modified files |
| `git add` | Supported | Stages modified files |
| `git reset` | Supported | ~6.5s index refresh |
| `git commit` | Supported | Watcher detects HEAD change; overlay reconciles stale entries |
| `git checkout` | Supported | Re-indexes tree, reconciles overlay, refreshes git index |
| `git fetch` | Supported | Background refresh loop fetches periodically |

### Known limitations

| Issue | Impact |
|-------|--------|
| `git status` takes ~7s on large repos (5800+ entries) | Performance -- full tree walk through FUSE |
| `git reset` takes ~6.5s for index refresh | Performance -- same root cause as `git status` |

## Testing

Unit tests:

```bash
go test ./...
```

End-to-end tests mount a git repo via FUSE and exercise filesystem + git operations (including commit and overlay reconciliation). They require a FUSE implementation (macFUSE on macOS, `fuse3` on Linux) and are off by default.

By default, e2e tests create a local bare repo -- no network required. Set `AFS_E2E_REPO` to test against a real remote.

```bash
# Run e2e tests (uses a local test repo by default)
AFS_RUN_E2E_TESTS=1 go test -v -run TestE2E -count=1 -timeout 10m .

# Run against a specific remote repo
AFS_RUN_E2E_TESTS=1 \
  AFS_E2E_REPO=https://github.com/cloudflare/workers-sdk.git \
  go test -v -run TestE2E -count=1 -timeout 10m .
```

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AFS_RUN_E2E_TESTS` | `0` | Set to `1` to enable end-to-end tests |
| `AFS_E2E_REPO` | local bare repo | Git remote URL for e2e tests. When unset, a local bare repo is created automatically. Set to an HTTPS URL to test against a real remote (accepts authenticated URLs). |
| `ARTIFACT_FS_ROOT` | `~/.local/share/artifact-fs` (macOS) or `/var/lib/artifact-fs` (Linux) | Runtime data root for the daemon and CLI |

## Contributing

Contributions are welcome, but not all contributions will be accepted. As guidance:

1. **Ensure you open an issue describing your change** - why it's a problem, how to reproduce it (if it's a bug)
2. **Your PR should be clear and concise** - including why it should be upstreamed.
3. **You are expected to have self-reviewed** - any PRs that are straight from automation with glaring issues, that don't build, or don't add good tests are likely to be closed.

AI/LLM submissions are welcome, but overall issue/PR quality is ultimately the responsibility of the submitter, and the codebase is the responsibility (and long term maintenance burden) of the maintainers.

See [AGENTS.md](AGENTS.md) for build commands, architecture details, and conventions. Run `go test ./...` and `go vet ./...` before submitting changes.

## Credits

The ArtifactFS FUSE driver takes inspiration from and draws from implementation details in:

* [TigrisFS](https://github.com/tigrisdata/tigrisfs/)
* [gitfs](https://github.com/presslabs/gitfs)
* [SlothFS](https://gerrit.googlesource.com/gitfs/)

## License

(c) Cloudflare, 2026. Apache-2.0 licensed.
