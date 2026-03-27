# AGENTS.md

Guide for AI coding agents working in this repo. Read this before making changes.

## Build and test

- `go build ./cmd/artifact-fs` -- build the binary
- `go test ./...` -- run all tests
- `go vet ./...` -- static analysis (no linter beyond vet)
- Tests live in `*_test.go` alongside source. Key packages: `internal/auth`, `internal/fusefs`, `internal/gitstore`, `internal/hydrator`

## Running the daemon

`add-repo` and `daemon` are separate commands with very different lifecycles. Do not confuse them.

- **`add-repo`** -- one-shot command. Clones the repo, registers it in SQLite, then exits. Does NOT mount FUSE or start goroutines.
- **`daemon`** -- long-running process. Mounts all registered repos via FUSE, starts background goroutines (watcher, hydrator), blocks on `<-ctx.Done()`.

Interactive test workflow:
```sh
export ARTIFACT_FS_ROOT=/tmp/artifact-fs-test
./artifact-fs add-repo --name myrepo --remote https://github.com/org/repo.git --branch main --mount-root /tmp
./artifact-fs daemon --root /tmp &
DAEMON_PID=$!
# test against /tmp/myrepo/
kill $DAEMON_PID
```

- Daemon logs JSON to stderr. Capture with `2>/tmp/daemon.log`.
- After killing the daemon, clean stale mounts with `umount /tmp/myrepo`.
- macFUSE must be installed (`/Library/Filesystems/macfuse.fs` must exist).

## Codebase structure

```
cmd/artifact-fs/main.go       -- entry point
internal/cli/cli.go            -- urfave/cli v1 command definitions
internal/daemon/daemon.go      -- service lifecycle, mountRepo/prepareRepo split
internal/fusefs/fuse_unix.go   -- FUSE adapter (ArtifactFuse), mount/unmount
internal/fusefs/merged.go      -- Resolver: merged view of snapshot + overlay
internal/fusefs/ops.go         -- Engine: read/write FUSE operations
internal/gitstore/gitstore.go  -- git subprocess wrapper (clone, fetch, ls-tree, cat-file)
internal/snapshot/store.go     -- SQLite-backed snapshot with generation publishing
internal/overlay/store.go      -- persistent writable overlay (SQLite + upper dir)
internal/hydrator/hydrator.go  -- priority queue blob fetcher with deduped waiters
internal/watcher/watcher.go    -- polls git HEAD/refs/index for changes
internal/registry/registry.go  -- repo config persistence
internal/model/types.go        -- shared types, interfaces, CleanPath
internal/meta/sqlite.go        -- SQLite helper (OpenDB with WAL + busy_timeout)
internal/auth/redact.go        -- token/URL redaction
internal/logging/logging.go    -- slog JSON handler with redaction
```

## Architecture

- **Resolver** (`fusefs/merged.go`) -- merges snapshot (base git tree) + overlay (local writes) into a unified view. Used by the FUSE adapter for Lookup/Getattr/Readdir.
- **Engine** (`fusefs/ops.go`) -- handles writes by promoting base files to the overlay via `ensureOverlay()` (hydrate blob, copy-on-write). Used by the FUSE adapter for Write/Create/Rename/Truncate.
- **Inode model** -- monotonic allocation at runtime (root = inode 1). NOT persisted in SQLite.
- **Snapshot generation** -- atomic int64, updated by the watcher when HEAD changes. Resolver reads it atomically so FUSE ops see the new tree without locks.
- **Interface consolidation** -- `model.OverlayStore` is the single canonical interface for overlay operations. Do not create subset interfaces in fusefs.
- **Path normalization** -- always use `model.CleanPath()`. Do not create local `cleanPath` functions.

## Conventions

- Keep `add-repo` non-blocking. It must not call `fuse.Mount` or start goroutines.
- Only `daemon` mounts FUSE and starts background goroutines.
- Never pass tokens as git CLI arguments. Use env-based credential helpers (see `credentialEnv` in `gitstore.go`).
- Binary-safe blob handling: `BlobToCache` pipes `git cat-file -p` stdout directly to a temp file. Never convert blob bytes to strings.
- `git ls-tree -l` hangs on blobless clones (triggers network fetches for blob sizes). Use `git ls-tree` without `-l`, then `git cat-file --batch-check --buffer` for sizes.
- SQLite: always use WAL mode + `busy_timeout=5000` (see `meta/sqlite.go`).

## Do not

- Add new `cleanPath` or path normalization functions. Use `model.CleanPath`.
- Create subset interfaces for overlay (e.g. `OverlayWriter`, `OverlayLookup`). Use `model.OverlayStore`.
- Use `git ls-tree -l` on blobless clones.
- Pass credentials in git command-line arguments.
- Call `fuse.Mount` from CLI commands other than `daemon`.
