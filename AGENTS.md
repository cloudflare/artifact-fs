# AGENTS.md

Read this before changing `artifact-fs`.

## Critical constraints

- Credentials must never appear in git CLI args. Use the env-based credential helper path in `internal/gitstore/gitstore.go` so tokens do not show up in `ps`, and keep log output redacted with `auth.RedactString`.
- Blob content must stay binary-safe. Do not convert blob bytes to `string`; `BlobToCache` streams `git cat-file --batch` output to disk for a reason.
- Keep `GIT_NO_LAZY_FETCH=1` on `git cat-file --batch-check` in `batchResolveSizes`. Without it, blobless clones turn size resolution into network round-trips.
- `model.CleanPath()` is the only path-normalization function. Do not add local wrappers.
- Do not reintroduce subset interfaces around snapshot, overlay, hydrator, or gitstore access. Use the canonical `model.*` interfaces.

## Commands that matter

- CI runs `go build ./cmd/artifact-fs`, then `go vet ./...`, then `go test ./...`. Follow that order for non-trivial changes.
- Build the CLI with `go build ./cmd/artifact-fs`.
- Run one package with `go test ./internal/<pkg>`.
- Run one test with `go test -run TestName ./internal/<pkg>`.
- Benchmarks are opt-in: `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v`.
- FUSE e2e tests are opt-in: `AFS_RUN_E2E_TESTS=1 go test -run TestE2E -v .`.
- E2E tests default to a local bare repo. Set `AFS_E2E_REPO` only when you intentionally want a real remote.
- E2E benchmark coverage is separate: `AFS_RUN_E2E_BENCH=1 go test -run TestE2EBenchmarkRepos -v .`.

## Repo shape

- `cmd/artifact-fs` is the only binary entrypoint.
- `internal/cli` wires commands onto `daemon.Service`.
- `internal/daemon` owns repo lifecycle: registry sync, snapshot publish, overlay reconcile, FUSE mount, watcher, refresh loop.
- `internal/fusefs` is the merged view and writable filesystem layer.
- `internal/gitstore` is the performance-sensitive git wrapper; most easy-to-break invariants live there.
- `internal/snapshot` and `internal/overlay` are persistent SQLite-backed stores.

## Non-obvious CLI/runtime behavior

- `ARTIFACT_FS_ROOT` is the state root. `artifact-fs daemon --root` is the mount root. They are different things.
- `add-repo` is one-shot by default: register repo, clone blobless, build the initial snapshot, then exit. It does not mount FUSE or start background goroutines.
- `add-repo --async` only registers prepare state. The daemon mounts a gated placeholder, prepares clone/fetch and snapshot in the background, then opens the gate and starts watcher/refresh.
- `daemon` is long-running: it mounts registered repos and starts watcher, refresh, and hydrator workers.
- `git.CloneBlobless` already populates the git index with `read-tree HEAD`; be careful about extra index resets because they can discard staged state.

## Testing and environment quirks

- macOS e2e coverage needs macFUSE. Linux e2e coverage needs `/dev/fuse`.
- Tests live next to code, but `bench_test.go`, `e2e_test.go`, `e2e_git_test.go`, and `e2e_bench_test.go` are in the root package.

## Current code conventions worth preserving

- `Readdir()` stays thin; merged directory logic belongs in `ReaddirTyped()`.
- The watcher polls `HEAD` plus the current HEAD ref path. If you change it, preserve branch-switch and packed-ref behavior covered by `internal/watcher/watcher_test.go`.
- The overlay stores deletes as SQLite entries with `kind='delete'`; there is no on-disk whiteout file layer.
- SQLite is `modernc.org/sqlite` in WAL mode via `internal/meta`.
