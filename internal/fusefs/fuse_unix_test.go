//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

func TestInodeAttrsPreservesSeparateTimes(t *testing.T) {
	mtime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctime := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)

	attr := inodeAttrs(0o644, 12, "file", mtime, ctime)
	if !attr.Atime.Equal(mtime) {
		t.Fatalf("atime = %v, want %v", attr.Atime, mtime)
	}
	if !attr.Mtime.Equal(mtime) {
		t.Fatalf("mtime = %v, want %v", attr.Mtime, mtime)
	}
	if !attr.Ctime.Equal(ctime) {
		t.Fatalf("ctime = %v, want %v", attr.Ctime, ctime)
	}
}

func TestInodeAttrsPreservesExplicitZeroDirMode(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	attr := inodeAttrs(0, 4096, "dir", now, now)
	if attr.Mode.Perm() != 0 {
		t.Fatalf("mode perms = %#o, want 0", attr.Mode.Perm())
	}
	if !attr.Mode.IsDir() {
		t.Fatalf("expected directory mode, got %#o", attr.Mode)
	}
}

func TestGitFileAttrsUsesOneTimestamp(t *testing.T) {
	fs := &ArtifactFuse{gitfileContent: []byte("gitdir: /tmp/repo/.git\n")}

	attr := fs.gitFileAttrs()
	if attr.Mtime.IsZero() || attr.Atime.IsZero() || attr.Ctime.IsZero() {
		t.Fatalf("expected non-zero times: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
	if !attr.Atime.Equal(attr.Mtime) || !attr.Ctime.Equal(attr.Mtime) {
		t.Fatalf("expected .git attrs to use one timestamp: atime=%v mtime=%v ctime=%v", attr.Atime, attr.Mtime, attr.Ctime)
	}
}

func TestLookUpInodeHydratesUnknownSizeBaseFileAttributes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}

	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	if op.Entry.Attributes.Size != uint64(h.size) {
		t.Fatalf("lookup size = %d, want hydrated size %d", op.Entry.Attributes.Size, h.size)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestGetInodeAttributesHydratesUnknownSizeBaseFileAttributes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	h.calls = 0

	op := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := fs.GetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("GetInodeAttributes: %v", err)
	}
	if op.Attributes.Size != uint64(h.size) {
		t.Fatalf("getattr size = %d, want hydrated size %d", op.Attributes.Size, h.size)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestGetInodeAttributesHydrationFailureReturnsEIO(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{
		RepoID:    repo.ID,
		Path:      "file.txt",
		Type:      "file",
		Mode:      0o100644,
		ObjectOID: "blob",
		SizeState: "unknown",
	}
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatalf("LookUpInode: %v", err)
	}
	h.err = errors.New("hydrate failed")

	op := &fuseops.GetInodeAttributesOp{Inode: lookup.Entry.Child}
	if err := fs.GetInodeAttributes(context.Background(), op); err != syscall.EIO {
		t.Fatalf("GetInodeAttributes err = %v, want EIO", err)
	}
}

func TestLookUpInodeDoesNotHydrateKnownOverlayDirOrSymlinkAttributes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	tests := []struct {
		name    string
		base    model.BaseNode
		overlay map[string]model.OverlayEntry
		want    uint64
	}{
		{
			name: "known base file",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 0},
			want: 0,
		},
		{
			name: "overlay file",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"},
			overlay: map[string]model.OverlayEntry{
				"file.txt": {Path: "file.txt", Kind: model.OverlayKindModify, Mode: 0o644, SizeBytes: 3},
			},
			want: 3,
		},
		{
			name: "base dir",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "dir", Mode: 0o40000, ObjectOID: "tree", SizeState: "unknown"},
			want: 4096,
		},
		{
			name: "base symlink",
			base: model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "symlink", Mode: 0o120000, ObjectOID: "blob", SizeState: "unknown"},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newResolver(
				&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": tt.base}},
				&fakeOverlay{entries: tt.overlay},
			)
			h := &fakeLookupHydrator{size: 12}
			fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
			op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}

			if err := fs.LookUpInode(context.Background(), op); err != nil {
				t.Fatalf("LookUpInode: %v", err)
			}
			if op.Entry.Attributes.Size != tt.want {
				t.Fatalf("lookup size = %d, want %d", op.Entry.Attributes.Size, tt.want)
			}
			if h.calls != 0 {
				t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
			}
		})
	}
}

type fakeLookupHydrator struct {
	size  int64
	calls int
	err   error
}

func (f *fakeLookupHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeLookupHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, _ model.BaseNode) (string, int64, error) {
	f.calls++
	if f.err != nil {
		return "", 0, f.err
	}
	return "", f.size, nil
}

func (f *fakeLookupHydrator) ReadBlob(_ context.Context, _ model.RepoConfig, _ model.BaseNode, _ int64) ([]byte, error) {
	return nil, nil
}

func (f *fakeLookupHydrator) QueueDepth(model.RepoID) int { return 0 }
