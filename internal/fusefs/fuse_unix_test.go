//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestReadDirPlusDefersUnknownSizeBaseFileLookup(t *testing.T) {
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
	if fs.pathToInode["file.txt"] != 0 {
		t.Fatal("unknown-size entry should require a separate lookup")
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

func TestRenameRetargetsOpenSourceHandle(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"old.txt": {Path: "old.txt", Kind: model.OverlayKindCreate, Mode: 0o644},
	}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}}, overlay)
	fs := NewArtifactFuse(repo, resolver, &Engine{Resolver: resolver, Overlay: overlay})
	fs.mu.Lock()
	ref := fs.allocInode("old.txt", "file", 0o644, 1)
	fh := &FileHandle{inode: ref, path: "old.txt"}
	fs.fileHandles[1] = fh
	fs.filesByPath["old.txt"] = map[fuseops.HandleID]*FileHandle{1: fh}
	fs.mu.Unlock()

	op := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "old.txt", NewParent: fuseops.RootInodeID, NewName: "new.txt"}
	if err := fs.Rename(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if path := fh.pathSnapshot(); path != "new.txt" {
		t.Fatalf("handle path = %q, want new.txt", path)
	}
	if fs.pathToInode["new.txt"] != ref.ID || fs.pathToInode["old.txt"] != 0 {
		t.Fatalf("inode paths not retargeted: %+v", fs.pathToInode)
	}
}

func TestRenameReplacementPreservesOpenOverlayDestination(t *testing.T) {
	tmp := t.TempDir()
	sourceBacking := filepath.Join(tmp, "source")
	destinationBacking := filepath.Join(tmp, "destination")
	if err := os.WriteFile(sourceBacking, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationBacking, []byte("destination"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"source.txt":      {Path: "source.txt", Kind: model.OverlayKindCreate, Mode: 0o644, BackingPath: sourceBacking},
		"destination.txt": {Path: "destination.txt", Kind: model.OverlayKindCreate, Mode: 0o644, BackingPath: destinationBacking},
	}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}}, overlay)
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, resolver, &Engine{Resolver: resolver, Overlay: overlay})
	fs.mu.Lock()
	sourceRef := fs.allocInode("source.txt", "file", 0o644, 1)
	destinationRef := fs.allocInode("destination.txt", "file", 0o644, 1)
	fs.mu.Unlock()
	sourceOpen := &fuseops.OpenFileOp{Inode: sourceRef.ID}
	if err := fs.OpenFile(context.Background(), sourceOpen); err != nil {
		t.Fatal(err)
	}
	destinationOpen := &fuseops.OpenFileOp{Inode: destinationRef.ID, OpenFlags: 2}
	if err := fs.OpenFile(context.Background(), destinationOpen); err != nil {
		t.Fatal(err)
	}

	rename := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "source.txt", NewParent: fuseops.RootInodeID, NewName: "destination.txt"}
	if err := fs.Rename(context.Background(), rename); err != nil {
		t.Fatal(err)
	}
	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: destinationRef.ID, N: 1}); err != nil {
		t.Fatal(err)
	}
	if fs.pathToInode["destination.txt"] != sourceRef.ID {
		t.Fatalf("destination mapping = %d, want source inode %d", fs.pathToInode["destination.txt"], sourceRef.ID)
	}
	read := &fuseops.ReadFileOp{Handle: destinationOpen.Handle, Size: 32}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if got := string(read.Data[0]); got != "destination" {
		t.Fatalf("replaced destination handle read = %q, want destination", got)
	}
	write := &fuseops.WriteFileOp{Handle: destinationOpen.Handle, Offset: 0, Data: []byte("D")}
	if err := fs.WriteFile(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destinationBacking)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "Destination" {
		t.Fatalf("detached destination content = %q, want Destination", data)
	}
}

func TestUnlinkPreservesOpenOverlayHandle(t *testing.T) {
	tmp := t.TempDir()
	backing := filepath.Join(tmp, "open")
	if err := os.WriteFile(backing, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"open.txt": {Path: "open.txt", Kind: model.OverlayKindCreate, Mode: 0o644, BackingPath: backing},
	}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}}, overlay)
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, resolver, &Engine{Resolver: resolver, Overlay: overlay})
	fs.mu.Lock()
	ref := fs.allocInode("open.txt", "file", 0o644, 1)
	fs.mu.Unlock()
	open := &fuseops.OpenFileOp{Inode: ref.ID, OpenFlags: 2}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	unlink := &fuseops.UnlinkOp{Parent: fuseops.RootInodeID, Name: "open.txt"}
	if err := fs.Unlink(context.Background(), unlink); err != nil {
		t.Fatal(err)
	}
	write := &fuseops.WriteFileOp{Handle: open.Handle, Offset: 0, Data: []byte("C")}
	if err := fs.WriteFile(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	read := &fuseops.ReadFileOp{Handle: open.Handle, Size: 16}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if got := string(read.Data[0]); got != "Content" {
		t.Fatalf("unlinked handle content = %q, want Content", got)
	}
	if entry, ok := overlay.Get("open.txt"); !ok || !entry.IsDeleted() {
		t.Fatalf("unlinked path was recreated: %+v, ok=%v", entry, ok)
	}
}

func TestRenameReplacementPreservesWritableBaseDestination(t *testing.T) {
	tmp := t.TempDir()
	baseCache := filepath.Join(tmp, "base-cache")
	sourceBacking := filepath.Join(tmp, "source")
	if err := os.WriteFile(baseCache, []byte("destination"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceBacking, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{
		"source.txt": {Path: "source.txt", Kind: model.OverlayKindCreate, Mode: 0o644, BackingPath: sourceBacking},
	}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{
		"destination.txt": {Path: "destination.txt", Type: "file", Mode: 0o644, ObjectOID: "base"},
	}}, overlay)
	repo := model.RepoConfig{ID: "repo", OverlayDir: filepath.Join(tmp, "overlay")}
	fs := NewArtifactFuse(repo, resolver, &Engine{Resolver: resolver, Repo: repo, Overlay: overlay, Hydrator: &fakeBatchHydrator{path: baseCache}})
	fs.mu.Lock()
	sourceRef := fs.allocInode("source.txt", "file", 0o644, 1)
	destinationRef := fs.allocInode("destination.txt", "file", 0o644, 1)
	fs.mu.Unlock()
	destinationOpen := &fuseops.OpenFileOp{Inode: destinationRef.ID, OpenFlags: 2}
	if err := fs.OpenFile(context.Background(), destinationOpen); err != nil {
		t.Fatal(err)
	}
	rename := &fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "source.txt", NewParent: fuseops.RootInodeID, NewName: "destination.txt"}
	if err := fs.Rename(context.Background(), rename); err != nil {
		t.Fatal(err)
	}
	write := &fuseops.WriteFileOp{Handle: destinationOpen.Handle, Offset: 0, Data: []byte("D")}
	if err := fs.WriteFile(context.Background(), write); err != nil {
		t.Fatal(err)
	}
	read := &fuseops.ReadFileOp{Handle: destinationOpen.Handle, Size: 32}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if got := string(read.Data[0]); got != "Destination" {
		t.Fatalf("detached base handle content = %q, want Destination", got)
	}
	cacheData, err := os.ReadFile(baseCache)
	if err != nil {
		t.Fatal(err)
	}
	if string(cacheData) != "destination" {
		t.Fatalf("base cache was modified: %q", cacheData)
	}
	_ = sourceRef
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
