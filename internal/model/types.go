package model

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type RepoID string

type RepoConfig struct {
	ID                RepoID
	Name              string
	MountRoot         string
	MountPath         string
	RemoteURL         string
	RemoteURLRedacted string
	Branch            string
	RefreshInterval   time.Duration
	GitDir            string
	OverlayDir        string
	BlobCacheDir      string
	MetaDBPath        string
	OverlayDBPath     string
	Enabled           bool
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
	HydratedBlobCount  int64
	HydratedBlobBytes  int64
	DirtyOverlay       bool
	State              string
}

// BaseNode represents a tracked entry from the git tree. Inode IDs are assigned
// at runtime by the FUSE layer (monotonic allocation, like tigrisfs).
type BaseNode struct {
	RepoID    RepoID
	Path      string
	Type      string // file, dir, symlink
	Mode      uint32
	ObjectOID string
	SizeState string // unknown, known
	SizeBytes int64
}

type OverlayKind string

const (
	OverlayKindCreate  OverlayKind = "create"
	OverlayKindModify  OverlayKind = "modify"
	OverlayKindDelete  OverlayKind = "delete"
	OverlayKindRename  OverlayKind = "rename"
	OverlayKindMkdir   OverlayKind = "mkdir"
	OverlayKindSymlink OverlayKind = "symlink"
)

type OverlayEntry struct {
	RepoID      RepoID
	Path        string
	Kind        OverlayKind
	BackingPath string
	Mode        uint32
	SizeBytes   int64
	MtimeUnixNs int64
	SourceOID   string
	TargetPath  string
}

func (e OverlayEntry) IsDeleted() bool {
	return e.Kind == OverlayKindDelete
}

func (e OverlayEntry) NodeType() string {
	switch e.Kind {
	case OverlayKindMkdir:
		return "dir"
	case OverlayKindSymlink:
		return "symlink"
	default:
		return "file"
	}
}

type HydrationTask struct {
	RepoID     RepoID
	Path       string
	ObjectOID  string
	Priority   int
	Reason     string
	EnqueuedAt time.Time
}

// CleanPath normalizes a filesystem path for use as a map key or DB lookup.
// It strips leading slashes, cleans dot-segments, and defaults empty/root to ".".
func CleanPath(path string) string {
	if path == "." || path == "/" || path == "" {
		return "."
	}
	path = filepath.Clean(path)
	if path == "/" {
		return "."
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "."
	}
	return path
}

// ValidateRepoName checks that a repo name is safe for use in filesystem paths.
func ValidateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repo name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("invalid repo name %q: must not contain path separators or '..'", name)
	}
	return nil
}

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
	CommitTimestamp(ctx context.Context, repo RepoConfig, oid string) (int64, error)
	ReadTreeHEAD(ctx context.Context, repo RepoConfig) error
}

type SnapshotStore interface {
	PublishGeneration(ctx context.Context, headOID string, ref string, nodes []BaseNode) (generation int64, err error)
	GetNode(generation int64, path string) (BaseNode, bool)
	ListChildren(generation int64, parentPath string) ([]BaseNode, error)
}

type OverlayStore interface {
	Get(path string) (OverlayEntry, bool)
	EnsureCopyOnWrite(ctx context.Context, repo RepoConfig, path string, base BaseNode) (OverlayEntry, error)
	CreateFile(ctx context.Context, path string, mode uint32) (OverlayEntry, error)
	WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error)
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	Mkdir(ctx context.Context, path string, mode uint32) error
	SetMtime(ctx context.Context, path string, t time.Time) error
	Reconcile(ctx context.Context, baseLookup func(path string) (BaseNode, bool)) error
	DirtyCount(ctx context.Context) (int64, error)
	ListByPrefix(ctx context.Context, prefix string) ([]OverlayEntry, error)
}

type Hydrator interface {
	Enqueue(task HydrationTask)
	EnsureHydrated(ctx context.Context, repo RepoConfig, node BaseNode) (cachePath string, size int64, err error)
	QueueDepth(repoID RepoID) int
}
