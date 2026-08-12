# AGENTS.md

Repository-specific guidance for `artifact-fs`.

## Code review rules

- Flag credentials in Git CLI arguments or unredacted logs. Pass credentials through the environment-based helper in `internal/gitstore/gitstore.go`, and redact log output with `auth.RedactString`.
- Flag blob bytes converted to `string`. Keep `BlobToCache` binary-safe by streaming `git cat-file --batch` output to disk.
- Keep `GIT_NO_LAZY_FETCH=1` on `git cat-file --batch-check` in `batchResolveSizes`. Removing it turns size resolution in blobless clones into network round-trips.
- Normalize paths only with `model.CleanPath()`. Do not add local wrappers.
- Use the canonical `model.*` interfaces for snapshot, overlay, hydrator, and gitstore access. Do not add subset interfaces around them.

## Architecture

- `cmd/artifact-fs` is the only binary entrypoint.
- `internal/cli` wires commands onto `daemon.Service`.
- `internal/daemon` owns repo lifecycle: registry sync, snapshot publish, overlay reconcile, FUSE mount, watcher, refresh loop.
- `internal/fusefs` is the merged view and writable filesystem layer.
- `internal/gitstore` is the performance-sensitive git wrapper; most easy-to-break invariants live there.
- `internal/snapshot` and `internal/overlay` are persistent SQLite-backed stores.

## Runtime contracts

- `ARTIFACT_FS_ROOT` is the state root. `artifact-fs daemon --root` is the mount root. They are different things.
- `add-repo` is one-shot by default: register repo, clone blobless, build the initial snapshot, then exit. It does not mount FUSE or start background goroutines.
- `add-repo --async` only registers prepare state. The daemon mounts a gated placeholder, prepares clone/fetch and snapshot in the background, then opens the gate and starts watcher/refresh.
- `daemon` is long-running: it mounts registered repos and starts watcher, refresh, and hydrator workers.
- `git.CloneBlobless` already populates the git index with `read-tree HEAD`; be careful about extra index resets because they can discard staged state.

## Code conventions

- `Readdir()` stays thin; merged directory logic belongs in `ReaddirTyped()`.
- The watcher polls `HEAD` plus the current HEAD ref path. If you change it, preserve branch-switch and packed-ref behavior covered by `internal/watcher/watcher_test.go`.
- The overlay stores deletes as SQLite entries with `kind='delete'`; there is no on-disk whiteout file layer.
- SQLite is `modernc.org/sqlite` in WAL mode via `internal/meta`.

## Validation

- Start with the narrowest relevant test: `go test ./internal/<pkg>` or `go test -run TestName ./internal/<pkg>`.
- For non-trivial code changes, reproduce CI in order: `go build ./cmd/artifact-fs`, `go vet ./...`, then `go test ./...`.
- Build only the CLI with `go build ./cmd/artifact-fs`.
- Benchmarks are opt-in: `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v`.
- FUSE e2e tests are opt-in: `AFS_RUN_E2E_TESTS=1 go test -run TestE2E -v .`. They require macFUSE on macOS or `/dev/fuse` on Linux.
- E2E tests use a local bare repository by default. Set `AFS_E2E_REPO` only when intentionally testing a real remote.
- E2E benchmark coverage is separate: `AFS_RUN_E2E_BENCH=1 go test -run TestE2EBenchmarkRepos -v .`.
- Package tests live beside the code. Benchmarks and `e2e*_test.go` live in the root package.
