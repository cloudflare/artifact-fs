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

func TestRootInodeAttributesDoNotRequireResolver(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{Name: "repo", GitDir: "/tmp/repo.git"}, nil, nil)
	op := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}

	if err := fs.GetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("GetInodeAttributes(root): %v", err)
	}
	if !op.Attributes.Mode.IsDir() {
		t.Fatalf("root mode = %#o, want directory", op.Attributes.Mode)
	}
	if op.Attributes.Size == 0 {
		t.Fatal("root size = 0, want non-zero placeholder size")
	}
}

func TestRootInodeAttributesUseStableResolverAttrsWhenReady(t *testing.T) {
	resolver := &Resolver{
		Snapshot: &fakeSnapshot{nodes: map[string]model.BaseNode{
			".": {Path: ".", Type: "dir", Mode: 0o755, SizeBytes: 4096},
		}},
		Overlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}},
	}
	resolver.SetGeneration(7)
	resolver.SetCommitTime(1_700_000_000)
	fs := NewArtifactFuse(model.RepoConfig{Name: "repo", GitDir: "/tmp/repo.git"}, resolver, nil)

	first := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
	if err := fs.GetInodeAttributes(context.Background(), first); err != nil {
		t.Fatalf("first GetInodeAttributes(root): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	second := &fuseops.GetInodeAttributesOp{Inode: fuseops.RootInodeID}
	if err := fs.GetInodeAttributes(context.Background(), second); err != nil {
		t.Fatalf("second GetInodeAttributes(root): %v", err)
	}

	want := time.Unix(1_700_000_000, 0)
	if !first.Attributes.Mtime.Equal(want) || !second.Attributes.Mtime.Equal(want) {
		t.Fatalf("root mtime = %v then %v, want stable %v", first.Attributes.Mtime, second.Attributes.Mtime, want)
	}
	if !first.Attributes.Ctime.Equal(second.Attributes.Ctime) {
		t.Fatalf("root ctime changed: %v then %v", first.Attributes.Ctime, second.Attributes.Ctime)
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

func TestReadDirPlusUsesReaddirMetadataWithoutHydration(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 4096)
	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead == 0 {
		t.Fatal("ReadDirPlus wrote no entries")
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
	if fs.pathToInode["file.txt"] == 0 {
		t.Fatal("ReadDirPlus did not allocate file inode")
	}
}

func TestReadDirPlusDoesNotHydrateUnknownSizeBaseFile(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "unknown"}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	h := &fakeLookupHydrator{size: 12}
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 4096)

	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead == 0 {
		t.Fatal("ReadDirPlus wrote no entries")
	}
	if h.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", h.calls)
	}
}

func TestReadDirPlusUsesOpenDirGeneration(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	snap := &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{}, kids: map[int64]map[string][]model.BaseNode{
		1: {".": {{Path: "old.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 1}}},
		2: {".": {{Path: "new.txt", Type: "file", Mode: 0o100644, SizeState: "known", SizeBytes: 2}}},
	}}
	r := &Resolver{Snapshot: snap, Overlay: &fakeOverlay{entries: map[string]model.OverlayEntry{}}}
	r.SetGeneration(1)
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeLookupHydrator{}})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	r.SetGeneration(2)
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 4096)

	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if fs.pathToInode["old.txt"] == 0 {
		t.Fatal("ReadDirPlus did not use OpenDir generation entry")
	}
	if fs.pathToInode["new.txt"] != 0 {
		t.Fatal("ReadDirPlus used live resolver generation entry")
	}
}

func TestReadDirPlusDropsLookupWhenEntryDoesNotFit(t *testing.T) {
	repo := model.RepoConfig{ID: "repo", GitDir: "/tmp/repo.git"}
	r := newResolver(
		&fakeSnapshot{kids: map[string][]model.BaseNode{
			".": {{Path: "file.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 42}},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: &fakeLookupHydrator{}})
	open := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
	if err := fs.OpenDir(context.Background(), open); err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	op := &fuseops.ReadDirPlusOp{}
	op.Handle = open.Handle
	op.Dst = make([]byte, 1)
	if err := fs.ReadDirPlus(context.Background(), op); err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if op.BytesRead != 0 {
		t.Fatalf("BytesRead = %d, want 0", op.BytesRead)
	}
	if fs.pathToInode[".git"] != 0 {
		t.Fatal("inode lookup leaked for entry that did not fit")
	}
}

type fakeLookupHydrator struct {
	size  int64
	calls int
	err   error
}

func (f *fakeLookupHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeLookupHydrator) EnqueueBatch([]model.HydrationTask) {}

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
