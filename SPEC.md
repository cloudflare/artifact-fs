ArtifactFS v1 — Implementation Specification
This document is the authoritative v1 build spec for ArtifactFS, a Git-backed FUSE filesystem with a persistent writable overlay.

ArtifactFS mounts one or more Git repositories under a mount root and exposes each repository as a normal working tree, while avoiding eager download of file contents. It relies on Git’s blobless partial clone support: git clone --filter=blob:none omits blobs initially, keeps commits and trees locally, and allows omitted objects to be fetched later from a promisor remote when needed. Git also supports a plain-text .git gitfile at the working-tree root that points to the real repository directory, which ArtifactFS uses so the mount behaves like a normal Git repo. Tree metadata gives names, modes, and object IDs, but exact regular-file sizes come from blob objects, so file sizes may be unknown before hydration. Later operations that move HEAD, such as checkout or merge, may require Git to download missing blobs; that is acceptable in v1. 

1. Goals
ArtifactFS must:

mount one or more repos under a shared mount root

show the full tracked path tree quickly after mount

avoid downloading blobs up front

lazily hydrate file content on first access or via background prioritization

provide a persistent writable overlay so agents can edit files normally

remain opaque to the agent; no special agent behavior is required

support normal Git commands inside the mounted repo

auto-fetch remote updates on a configurable timer

never change checked-out branch state underneath the agent

Example mount layout:

/mnt/
  repo-a/
  repo-b/
2. Fixed product decisions
These are mandatory for v1.

Included
writable overlay

persistent untracked files and local modifications

HTTPS-token remotes only

separate CLI for config and control

timer-driven fetch of remotes

normal Git working tree presentation

prioritization for agent/code-reading workloads

Excluded
submodules

Git LFS

custom virtual branch paths

automatic branch advancement on fetch

exact file sizes before hydration

control files inside mounted repos

Acceptable v1 limitation
git switch, git checkout, merge, and similar operations may be slower because Git may need to fetch missing blobs during those workflows. 

3. User-visible contract
Each mounted repo must look like a normal Git working tree at:

<mount-root>/<repo-name>
Inside each repo root:

tracked directories and files from current HEAD are visible immediately after snapshot build

local edits go to a persistent overlay

Git commands operate normally

remote updates are fetched periodically, but the working tree remains on the branch or commit chosen by the user or agent

.git is exposed as a gitfile pointing at the real Git dir

ArtifactFS is not a sync engine for unstaged files. Local edits exist in the overlay until the user or agent stages and commits them using Git.

4. Core model
For each repo, ArtifactFS manages four layers:

visible_tree = overlay ⊕ base_snapshot
Where:

base_snapshot is derived from the current HEAD tree in the real Git repo

overlay contains local creates/modifies/deletes/renames

overlay shadows base snapshot entries

missing base blobs are hydrated lazily into a blob cache

Components
Git store

real local Git repository

partial clone, blobless

real refs, index, objects, commits

Snapshot index

current HEAD tree, fully enumerated

path lookup index

inode map

cached metadata

Overlay store

writable upper layer

whiteouts/tombstones

persistent metadata

Hydrator

background blob-fetch scheduler

demand fetch on file access

prioritization for text/code/bootstrap files

5. Required Git layout
ArtifactFS must create and manage a real Git repo on disk.

Recommended repo bootstrap:

git clone --filter=blob:none --no-checkout --single-branch <remote-url> <repo-git-parent>
Then configure working-tree behavior around that repo.

This is required because blobless partial clone keeps commits and trees while filtering out file contents until needed. 

Mounted repo root must expose:

.git
as a gitfile with contents:

gitdir: /var/lib/artifact-fs/repos/<repo-id>/git
Git supports this gitfile mechanism for working trees whose repository directory lives elsewhere. 

6. On-disk layout
Default root:

/var/lib/artifact-fs/
Layout:

/var/lib/artifact-fs/
  repos/
    <repo-id>/
      git/                  # real .git directory contents
  overlays/
    <repo-id>/
      upper/                # persistent writable files
      whiteouts/            # tombstones / deletions
      meta.sqlite           # overlay metadata
  cache/
    blobs/
      <repo-id>/
        <oid>               # hydrated blob content
  meta/
    <repo-id>.sqlite        # repo metadata + snapshot index + learned file sizes
  config/
    repos.sqlite            # registry of configured repos
  logs/
Requirements:

all overlay content survives restart and remount

blob cache is evictable

overlay content is never evicted automatically

logs must not contain tokens

7. Process model
Single daemon process with worker pools.

Subsystems
registry: configured repos and mount state

gitstore: clone/fetch/ref/index/object operations

snapshot: current HEAD tree index

overlay: local writes and reconciliation

fusefs: FUSE operations

hydrator: priority queue and fetch workers

watcher: detect local Git state changes

cli: external control surface

8. Language and implementation choices
v1 target language: Go

Reasoning:

stable process model

good concurrency primitives

straightforward SQLite integration

good fit for daemon + worker pools + FUSE bindings

Do not implement Git transport yourself in v1. Use the system git binary for:

clone

fetch

ref resolution

tree enumeration

object existence checks

blob materialization

9. Repository structure
Recommended repo layout:

artifact-fs/
  cmd/
    artifact-fs/
      main.go
  internal/
    cli/
    daemon/
    registry/
    fusefs/
    gitstore/
    snapshot/
    overlay/
    hydrator/
    watcher/
    meta/
    logging/
    auth/
  migrations/
  testdata/
  scripts/
10. Data model
10.1 Go structs
type RepoID string

type RepoConfig struct {
    ID               RepoID
    Name             string
    MountRoot        string
    MountPath        string
    RemoteURL        string // stored encrypted or redacted-at-rest policy if implemented later
    RemoteURLRedacted string
    Branch           string
    RefreshInterval  time.Duration
    GitDir           string
    OverlayDir       string
    BlobCacheDir     string
    MetaDBPath       string
    OverlayDBPath    string
    Enabled          bool
}

type RepoRuntimeState struct {
    RepoID             RepoID
    CurrentHEADOID     string
    CurrentHEADRef     string
    SnapshotGeneration int64
    LastFetchAt        time.Time
    LastFetchResult    string
    AheadCount         int
    BehindCount        int
    Diverged           bool
    DirtyOverlay       bool
    State              string // cloning, mounting, ready, fetching, reloading, degraded, error
}

type BaseNode struct {
    RepoID        RepoID
    Generation    int64
    Path          string
    Inode         uint64
    ParentInode   uint64
    Type          string // file, dir, symlink
    Mode          uint32
    ObjectOID     string
    SizeState     string // unknown, known
    SizeBytes     int64
}

type OverlayEntry struct {
    RepoID        RepoID
    Path          string
    Kind          string // create, modify, delete, rename, mkdir, symlink
    BackingPath   string
    Mode          uint32
    SizeBytes     int64
    MtimeUnixNs   int64
    SourceOID     string
    TargetPath    string // for rename
}

type HydrationTask struct {
    RepoID        RepoID
    Path          string
    ObjectOID     string
    Priority      int
    Reason        string
    EnqueuedAt    time.Time
}

type LearnedPathStats struct {
    RepoID        RepoID
    Path          string
    AccessCount   int64
    LastAccessNs  int64
    LastHydratedNs int64
}
11. SQLite schema
11.1 Global repo registry
CREATE TABLE repos (
  repo_id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  mount_root TEXT NOT NULL,
  mount_path TEXT NOT NULL,
  remote_url_redacted TEXT NOT NULL,
  remote_url_secret_ref TEXT,
  branch TEXT NOT NULL,
  refresh_interval_seconds INTEGER NOT NULL,
  git_dir TEXT NOT NULL,
  overlay_dir TEXT NOT NULL,
  blob_cache_dir TEXT NOT NULL,
  meta_db_path TEXT NOT NULL,
  overlay_db_path TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at_ns INTEGER NOT NULL,
  updated_at_ns INTEGER NOT NULL
);
11.2 Repo metadata DB
CREATE TABLE repo_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE base_nodes (
  generation INTEGER NOT NULL,
  path TEXT NOT NULL,
  inode INTEGER NOT NULL,
  parent_inode INTEGER NOT NULL,
  type TEXT NOT NULL,
  mode INTEGER NOT NULL,
  object_oid TEXT NOT NULL,
  size_state TEXT NOT NULL,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (generation, path)
);

CREATE INDEX idx_base_nodes_generation_inode ON base_nodes(generation, inode);
CREATE INDEX idx_base_nodes_generation_parent ON base_nodes(generation, parent_inode);

CREATE TABLE learned_path_stats (
  path TEXT PRIMARY KEY,
  access_count INTEGER NOT NULL DEFAULT 0,
  last_access_ns INTEGER NOT NULL DEFAULT 0,
  last_hydrated_ns INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE blob_cache_index (
  object_oid TEXT PRIMARY KEY,
  cache_path TEXT NOT NULL,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  last_access_ns INTEGER NOT NULL DEFAULT 0
);
11.3 Overlay DB
CREATE TABLE overlay_entries (
  path TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  backing_path TEXT,
  mode INTEGER NOT NULL,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  mtime_unix_ns INTEGER NOT NULL,
  source_oid TEXT,
  target_path TEXT
);

CREATE INDEX idx_overlay_kind ON overlay_entries(kind);
12. Inode model
Inodes must be deterministic within a snapshot generation.

Recommended strategy:

inode for directories/files in base snapshot:

hash(repo-id + generation + path)

inode for overlay-only entries:

hash(repo-id + "overlay" + path)

Requirements:

stable for duration of one mounted generation

may change across generation swaps

FUSE correctness matters more than inode persistence across branch switches

13. Snapshot construction
13.1 Source of truth
Base snapshot is always built from the current HEAD tree in the real Git repo.

Required behavior
resolve current HEAD

enumerate full tree recursively

create BaseNode rows

swap snapshot generation atomically

Recommended implementation
Use Git tree enumeration via git ls-tree recursively.

Git tree listings expose object mode, type, object ID, and path, and blob object size is a separate field derived from the blob object, not from the tree itself. 

13.2 File size policy
For base snapshot files:

if cached size is known, return it

otherwise return 0

do not fetch blobs solely to answer getattr

This is mandatory for v1.

14. Overlay semantics
14.1 Rules
Base snapshot is immutable.

All local mutations go to overlay.

Existing tracked file, first write
create overlay file

hydrate base blob if needed

copy content into overlay file

apply write

mark overlay entry as modify

New file
create overlay file

create overlay entry as create

Delete tracked file
create whiteout/tombstone

if overlay file exists for path, remove or preserve internal backing file as implementation chooses

visible path disappears

Rename tracked file
ensure source represented in overlay

create target overlay entry

create source tombstone

update metadata atomically

Directory operations
new dir: overlay metadata entry

remove dir: only if empty in visible merged tree

14.2 Whiteouts
Whiteouts must shadow both:

base snapshot nodes

overlay-created nodes after deletion

15. Merge view resolution
All path-based operations use this logic:

func ResolvePath(path string) ResolvedNode {
    if overlay.HasWhiteout(path) {
        return NotFound
    }
    if n, ok := overlay.Get(path); ok {
        return n
    }
    if n, ok := snapshot.Get(path); ok {
        return n
    }
    return NotFound
}
16. FUSE operation contract
ArtifactFS must implement:

Lookup

Getattr

Readdir

Open

Read

Write

Create

Unlink

Rename

Mkdir

Rmdir

Setattr / truncate

Readlink

Symlink

Flush

Release

Forget

Statfs

16.1 Rules by operation
Lookup
must not trigger hydration

use merged view resolution only

Getattr
directories: immediate

symlinks: immediate

overlay-backed files: exact size from overlay

base files: cached size if known, else 0

must not hydrate blob

Readdir
merge children from base snapshot and overlay

exclude whiteouts

must not hydrate blob

Open
if overlay-backed, open overlay file

if base-backed:

if blob cached, open cache file

else enqueue hydration and return a handle that blocks reads until ready or timeout

Read
overlay-backed: read overlay file

base-backed cached: read cache

base-backed uncached: wait on hydration future

Write
must always write to overlay file

copy-on-write if first write to tracked base file

Unlink
create whiteout

remove overlay file if present

Rename
implement as overlay metadata transaction

preserve content

Truncate
overlay only; promote tracked file to overlay if needed

17. Blocking semantics
Only file-content access may block on hydration.

These operations must remain non-blocking with respect to blob downloads:

Lookup

Getattr for non-overlay base files

Readdir

path traversal

mount startup after snapshot is built

This is a hard invariant.

18. Hydration subsystem
18.1 Purpose
Hydration fetches missing base blobs into local blob cache.

It never writes user modifications.

18.2 Task queue
Priority queue with worker pool.

Priority tiers, highest first:

explicit read/open request

sibling files in recently traversed directory

bootstrap/config/source files

small likely-text files

nearby code files

low-priority binaries/assets

18.3 Heuristic classifier
Boost:

README*

LICENSE*

Makefile

.gitignore

.github/**

go.mod, go.sum

Cargo.toml

package.json

pnpm-lock.yaml

pyproject.toml

requirements*.txt

src/**, cmd/**, pkg/**, lib/**

Boost extensions:

.go

.zig

.rs

.py

.ts

.tsx

.js

.jsx

.java

.c

.cc

.cpp

.h

.hpp

.json

.yaml

.yml

.toml

.md

Penalize:

images

videos

archives

PDFs

vendored binaries

generated assets

18.4 Worker behavior
For each hydration task:

verify blob still needed or worthwhile

fetch or materialize blob through Git/object path

store blob in cache by OID

update learned size and access metadata

wake blocked readers

18.5 Cache behavior
Blob cache is keyed by object OID.

Required:

persistent across restart

evictable by LRU or size target

overlay content never evicted

19. Git integration contract
Git is the source of truth for:

refs

HEAD

index

commits

object IDs

ArtifactFS is the source of truth for:

merged visible tree

overlay writes

hydration scheduling

mount behavior

19.1 Required Git commands to support in mounted repo
git status

git diff

git add

git restore

git reset

git commit

git branch

git switch

git checkout

19.2 Required local Git state detection
ArtifactFS must observe changes to:

HEAD

refs under the Git dir

index file

On detected HEAD change:

resolve new HEAD

rebuild base snapshot

atomically swap snapshot generation

reconcile overlay

On index-only change:

no snapshot rebuild required

status output may change

20. Overlay reconciliation after HEAD change
When local Git changes HEAD due to checkout, switch, reset, or commit:

build new base snapshot from new HEAD

preserve overlay entries

for each overlay entry:

if delete and path absent in new base: keep whiteout only if needed to hide overlay-created state

if modify and overlay content equals new base blob: collapse overlay entry

if create: keep

if rename: preserve target and source tombstone

publish new generation atomically

Equality check
To collapse a modified overlay entry, compare overlay content hash against new base blob content hash after hydration if necessary.

Optimization: defer expensive equality collapse until later if needed.

21. Refresh behavior
Each repo has a configurable refresh interval.

On each timer tick:

run git fetch origin

update remote-tracking refs

compute ahead/behind/diverged status

record timestamps and result

do not change local checked-out branch

do not modify working tree

do not reset overlay

This is mandatory.

22. Status model
CLI status must report:

repo name

mount path

current HEAD OID

current HEAD ref or detached state

upstream ref

ahead count

behind count

diverged bool

last fetch time

last fetch result

overlay dirty bool

overlay entry count

hydration queue depth

blob cache size

current daemon state

23. Security requirements
Because remotes use HTTPS tokens:

never log raw remote URL

redact token in all human-visible output

avoid including tokenized URLs in persistent config where possible

never echo tokenized URLs in errors

scrub subprocess stderr/stdout before logging

expose only redacted remote URL in status and metadata

24. CLI contract
Binary name:

artifact-fs
Required commands:

artifact-fs daemon --root /mnt

artifact-fs add-repo \
  --name repo-a \
  --remote https://TOKEN@host/org/repo-a.git \
  --branch main \
  --refresh 30s

artifact-fs remove-repo --name repo-a
artifact-fs list-repos
artifact-fs status --name repo-a
artifact-fs fetch --name repo-a
artifact-fs remount --name repo-a
artifact-fs unmount --name repo-a
artifact-fs evict-cache --name repo-a
artifact-fs gc --name repo-a
artifact-fs doctor --name repo-a
Optional:

artifact-fs prefetch --name repo-a --path path/to/dir
artifact-fs set-refresh --name repo-a --interval 1m
No configuration surface may appear inside the mounted repo.

25. Interfaces
These are the minimum internal interfaces.

type Registry interface {
    AddRepo(ctx context.Context, cfg RepoConfig) error
    RemoveRepo(ctx context.Context, name string) error
    GetRepo(ctx context.Context, name string) (RepoConfig, error)
    ListRepos(ctx context.Context) ([]RepoConfig, error)
}

type GitStore interface {
    CloneBlobless(ctx context.Context, cfg RepoConfig) error
    Fetch(ctx context.Context, repo RepoConfig) error
    ResolveHEAD(ctx context.Context, repo RepoConfig) (oid string, ref string, err error)
    BuildTreeIndex(ctx context.Context, repo RepoConfig, headOID string) ([]BaseNode, error)
    BlobToCache(ctx context.Context, repo RepoConfig, objectOID string, dstPath string) (size int64, err error)
    ComputeAheadBehind(ctx context.Context, repo RepoConfig) (ahead int, behind int, diverged bool, err error)
}

type SnapshotStore interface {
    PublishGeneration(ctx context.Context, repoID RepoID, headOID string, ref string, nodes []BaseNode) (generation int64, err error)
    GetNode(repoID RepoID, generation int64, path string) (BaseNode, bool)
    ListChildren(repoID RepoID, generation int64, path string) ([]BaseNode, error)
}

type OverlayStore interface {
    Get(path string) (OverlayEntry, bool)
    HasWhiteout(path string) bool
    EnsureCopyOnWrite(ctx context.Context, repo RepoConfig, path string, base BaseNode) (OverlayEntry, error)
    CreateFile(ctx context.Context, path string, mode uint32) (OverlayEntry, error)
    WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error)
    Remove(ctx context.Context, path string) error
    Rename(ctx context.Context, oldPath, newPath string) error
    Mkdir(ctx context.Context, path string, mode uint32) error
    Reconcile(ctx context.Context, newGeneration int64) error
    DirtyCount(ctx context.Context) (int64, error)
}

type Hydrator interface {
    Enqueue(task HydrationTask)
    EnsureHydrated(ctx context.Context, repo RepoConfig, path string, oid string) (cachePath string, size int64, err error)
    QueueDepth(repoID RepoID) int
}
26. Startup flow
For daemon startup:

load repo registry

initialize logging and DB connections

for each enabled repo:

verify real Git dir exists, else clone

resolve current HEAD

build snapshot

initialize overlay

mount repo path

start watchers

start refresh timer

start hydrator workers

mark repo ready

27. Add-repo flow
add-repo
  -> persist config
  -> clone blobless
  -> resolve HEAD
  -> build base snapshot
  -> init overlay store
  -> mount repo
  -> start repo workers
  -> ready
If any step fails:

report failure

clean partial mount

retain partial clone only if safe and explicitly desired

28. Watcher behavior
ArtifactFS must detect local Git state changes without polling the whole repo tree.

Watch at minimum:

HEAD

refs directory or targeted ref files

index file

On HEAD or ref change:

debounce briefly

resolve current HEAD

if changed, rebuild snapshot

On index change only:

no snapshot rebuild required

update runtime status only

29. Error handling
Required behavior
auth failure during fetch: mark repo degraded, leave current mount usable

hydration failure for one file: fail that file open/read, do not poison mount

snapshot rebuild failure: keep previous generation mounted

overlay DB corruption: fail repo mount loudly; do not continue with partial overlay semantics

blob cache corruption: delete corrupted cache entry and retry once

force-push upstream: update remote-tracking refs only; do not rewrite current worktree

Return codes:

ENOENT for missing paths

EROFS never used for normal file writes because overlay is writable

EIO or timeout-related errors for unrecoverable hydration failure

30. Logging and metrics
Logs
Include:

repo name

operation

latency

generation number

hydration queue events

fetch results

snapshot rebuild events

Never include:

tokens

full remote URL

raw blob content

Metrics
Recommended counters:

snapshot_build_seconds

hydration_queue_depth

hydration_success_total

hydration_failure_total

cache_hit_total

cache_miss_total

overlay_dirty_entries

fetch_success_total

fetch_failure_total

31. Testing requirements
Implement unit, integration, and end-to-end tests.

31.1 Unit tests
path merge resolution

overlay whiteout semantics

inode generation stability within generation

hydration priority ordering

token redaction

31.2 Integration tests
blobless clone bootstrap

snapshot build from real Git repo

on-demand blob hydration

overlay create/modify/delete/rename persistence

HEAD change detection and snapshot rebuild

timed fetch without branch advancement

31.3 End-to-end tests
Inside mounted repo:

git status

edit tracked file

git add

git commit

git switch

reopen daemon

confirm persistence and correctness

32. Acceptance criteria
ArtifactFS v1 is complete when all of the following are true:

mounting a large repo does not download all blobs

tracked tree appears quickly after mount

metadata ops do not block on hydration

first cold read hydrates only the needed file or a small prioritized batch

local edits work through the overlay using ordinary file operations

overlay survives restart

Git commands work from inside the mount

timed fetch updates remote-tracking refs only

checked-out branch is never advanced automatically

tokens are redacted everywhere user-visible

33. Ordered implementation task list
This is the build plan the coding agent should execute.

Phase 0 — Bootstrap
Create Go module artifact-fs.

Scaffold CLI with subcommands.

Add logging package with token redaction helper.

Add SQLite migration runner.

Add config/registry DB.

Phase 1 — Git substrate
Implement gitstore.CloneBlobless.

Implement gitstore.ResolveHEAD.

Implement gitstore.Fetch.

Implement gitstore.ComputeAheadBehind.

Implement redacted remote URL handling.

Add tests for clone/fetch/head resolution.

Phase 2 — Snapshot index
Implement tree walk from current HEAD.

Build BaseNode list.

Persist nodes by generation.

Implement fast lookup by path.

Implement child listing by directory.

Add deterministic inode generation.

Add snapshot generation publish/swap.

Add tests for nested trees and symlinks.

Phase 3 — Minimal read-only FUSE
Implement mount manager.

Implement Lookup.

Implement Getattr.

Implement Readdir.

Expose .git gitfile.

Mount a single repo read-only from snapshot metadata.

Add integration test that browses mounted repo.

Phase 4 — Blob hydration
Implement blob cache layout.

Implement gitstore.BlobToCache.

Implement hydrator queue and workers.

Implement Open and Read for base files.

Add blocking-per-file hydration futures.

Add learned size updates after hydration.

Add tests for cold read and concurrent unrelated metadata ops.

Phase 5 — Persistent overlay
Implement overlay SQLite schema.

Implement overlay file backing store.

Implement whiteouts.

Implement copy-on-write promotion.

Implement Write, Create, Unlink, Rename, Mkdir, Rmdir, Truncate.

Implement merged view resolver.

Add persistence tests across remount and restart.

Phase 6 — Git interoperability
Ensure .git gitfile and worktree semantics are correct.

Implement watcher for HEAD.

Implement watcher for refs.

Implement watcher for index.

Implement snapshot rebuild on HEAD change.

Implement overlay reconciliation after generation swap.

Test git status, git add, git commit, git switch.

Phase 7 — Refresh and status
Implement per-repo refresh timer.

Implement fetch-without-advance behavior.

Record ahead/behind/diverged.

Implement artifact-fs status.

Implement artifact-fs fetch.

Implement degraded/error states.

Add tests for dirty overlay + timed fetch.

Phase 8 — Prioritization quality
Implement hydration priority classifier.

Track path access statistics.

Prefetch siblings in recently traversed dirs.

Implement optional prefetch CLI.

Add cache eviction strategy.

Benchmark code-reading workloads.

Phase 9 — Hardening
Add token redaction tests across all command output.

Add corruption handling for cache and DB failure paths.

Add force-push scenario tests.

Add concurrent access stress tests.

Write developer docs and runbook.

34. Pseudocode for critical flows
34.1 Read path
func ReadPath(ctx context.Context, repo RepoConfig, path string, off int64, size int) ([]byte, error) {
    if overlay.HasWhiteout(path) {
        return nil, fs.ErrNotExist
    }

    if ov, ok := overlay.Get(path); ok {
        return readOverlayFile(ov.BackingPath, off, size)
    }

    node, ok := snapshot.GetCurrent(path)
    if !ok {
        return nil, fs.ErrNotExist
    }

    cachePath, _, err := hydrator.EnsureHydrated(ctx, repo, path, node.ObjectOID)
    if err != nil {
        return nil, err
    }
    return readFile(cachePath, off, size)
}
34.2 First write to tracked file
func EnsureWritable(ctx context.Context, repo RepoConfig, path string) (OverlayEntry, error) {
    if ov, ok := overlay.Get(path); ok {
        return ov, nil
    }

    node, ok := snapshot.GetCurrent(path)
    if !ok {
        return overlay.CreateFile(ctx, path, 0644)
    }

    cachePath, _, err := hydrator.EnsureHydrated(ctx, repo, path, node.ObjectOID)
    if err != nil {
        return OverlayEntry{}, err
    }

    return overlay.PromoteFromBase(ctx, path, node, cachePath)
}
34.3 HEAD change handling
func OnHEADChanged(ctx context.Context, repo RepoConfig) error {
    oid, ref, err := gitstore.ResolveHEAD(ctx, repo)
    if err != nil {
        return err
    }

    nodes, err := gitstore.BuildTreeIndex(ctx, repo, oid)
    if err != nil {
        return err
    }

    gen, err := snapshot.PublishGeneration(ctx, repo.ID, oid, ref, nodes)
    if err != nil {
        return err
    }

    if err := overlay.Reconcile(ctx, gen); err != nil {
        return err
    }

    runtime.UpdateGeneration(repo.ID, gen, oid, ref)
    return nil
}
35. Non-negotiable invariants
ArtifactFS must never advance the checked-out branch on its own.

Metadata operations must not require blob downloads.

Overlay content must survive restart.

Git remains the source of truth for refs, index, and commits.

Mounted repo must appear to clients as a normal Git working tree.

Control and config must exist only in the CLI, not in the mount.

Credentials must always be redacted from user-visible surfaces.

36. Implementation stop conditions
The agent should consider v1 complete only when:

all acceptance criteria are met

all Phase 0–9 tasks are done

end-to-end Git workflows pass against the mounted repo

restart persistence and non-advancing refresh behavior are verified

token redaction is verified in logs and CLI output

This is the full v1 implementation contract for ArtifactFS.