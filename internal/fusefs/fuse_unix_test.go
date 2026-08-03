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

func TestDetachedFileHandleRemainsReadableWritableAndSyncable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unlinked")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fh := &FileHandle{path: "file.txt", cacheFile: f, cacheGeneration: -1, detached: true}
	fs := &ArtifactFuse{fileHandles: map[fuseops.HandleID]*FileHandle{1: fh}}

	write := &fuseops.WriteFileOp{Handle: 1, Offset: 0, Data: []byte("after!")}
	if err := fs.WriteFile(context.Background(), write); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	read := &fuseops.ReadFileOp{Handle: 1, Offset: 0, Size: 6}
	if err := fs.ReadFile(context.Background(), read); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(read.Data[0]); got != "after!" {
		t.Fatalf("read = %q, want after!", got)
	}
	if err := fs.SyncFile(context.Background(), &fuseops.SyncFileOp{Handle: 1}); err != nil {
		t.Fatalf("SyncFile: %v", err)
	}
	if err := fs.ReleaseFileHandle(context.Background(), &fuseops.ReleaseFileHandleOp{Handle: 1}); err != nil {
		t.Fatalf("ReleaseFileHandle: %v", err)
	}
}

func TestWriteThroughOpenedBaseHandlePromotesAndWrites(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "file", Mode: 0o644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	snap := &fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": base}}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(snap, ov)
	h := &fakeLookupHydrator{size: 4, path: cachePath}
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: h}
	fs := NewArtifactFuse(repo, resolver, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	open := &fuseops.OpenFileOp{Inode: lookup.Entry.Child}
	if err := fs.OpenFile(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(context.Background(), &fuseops.WriteFileOp{Handle: open.Handle, Data: []byte("local")}); err != nil {
		t.Fatal(err)
	}
	if ov.writes != 1 || string(ov.data) != "local" {
		t.Fatalf("overlay writes = %d, data = %q", ov.writes, ov.data)
	}
}

func TestSetInodeAttributesAppliesExecutableBit(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(cachePath, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := model.RepoConfig{ID: "repo"}
	base := model.BaseNode{RepoID: repo.ID, Path: "script.sh", Type: "file", Mode: 0o100644, ObjectOID: "blob", SizeState: "known", SizeBytes: 4}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{base.Path: base}}, ov)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov, Hydrator: &fakeLookupHydrator{size: 4, path: cachePath}}
	fs := NewArtifactFuse(repo, resolver, engine)
	lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: base.Path}
	if err := fs.LookUpInode(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o755)
	op := &fuseops.SetInodeAttributesOp{Inode: lookup.Entry.Child, Mode: &mode}
	if err := fs.SetInodeAttributes(context.Background(), op); err != nil {
		t.Fatalf("SetInodeAttributes: %v", err)
	}
	if op.Attributes.Mode.Perm() != 0o755 {
		t.Fatalf("returned mode = %#o, want 0755", op.Attributes.Mode.Perm())
	}
	entry, ok := ov.entries[base.Path]
	if !ok || entry.Mode != 0o100755 {
		t.Fatalf("overlay entry = %+v, want promoted executable file", entry)
	}
}

func TestCreateAndReadOverlaySymlink(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	ov := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{}}, ov)
	engine := &Engine{Resolver: resolver, Repo: repo, Overlay: ov}
	fs := NewArtifactFuse(repo, resolver, engine)
	create := &fuseops.CreateSymlinkOp{
		Parent: fuseops.RootInodeID,
		Name:   "readme-link",
		Target: "README.md",
	}
	if err := fs.CreateSymlink(context.Background(), create); err != nil {
		t.Fatalf("CreateSymlink: %v", err)
	}
	if create.Entry.Attributes.Mode&os.ModeSymlink == 0 {
		t.Fatalf("created mode = %#o, want symlink", create.Entry.Attributes.Mode)
	}
	read := &fuseops.ReadSymlinkOp{Inode: create.Entry.Child}
	if err := fs.ReadSymlink(context.Background(), read); err != nil {
		t.Fatalf("ReadSymlink: %v", err)
	}
	if read.Target != create.Target {
		t.Fatalf("target = %q, want %q", read.Target, create.Target)
	}
}

func TestMoveInodePathPreservesSourceIdentityAndStalesReplacement(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{}, nil, nil)
	fs.mu.Lock()
	source := fs.allocInode("source.txt", "file", 0o100644, 1)
	replaced := fs.allocInode("destination.txt", "file", 0o100644, 1)
	fs.mu.Unlock()

	fs.moveInodePath("source.txt", "destination.txt")

	moved, err := fs.requireInode(source.ID, syscall.ESTALE)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Path != "destination.txt" {
		t.Fatalf("source inode path = %q, want destination.txt", moved.Path)
	}
	if got := fs.pathToInode["destination.txt"]; got != source.ID {
		t.Fatalf("destination inode = %d, want source %d", got, source.ID)
	}
	if _, err := fs.requireInode(replaced.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced inode error = %v, want ESTALE", err)
	}

	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: replaced.ID, N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := fs.pathToInode["destination.txt"]; got != source.ID {
		t.Fatalf("forgetting replaced inode removed source mapping: got %d, want %d", got, source.ID)
	}
}

func TestMoveInodePathRewritesDirectoryDescendants(t *testing.T) {
	fs := NewArtifactFuse(model.RepoConfig{}, nil, nil)
	fs.mu.Lock()
	source := fs.allocInode("source", "dir", 0o755, 1)
	child := fs.allocInode("source/nested/file.txt", "file", 0o100644, 1)
	sibling := fs.allocInode("source-other/file.txt", "file", 0o100644, 1)
	replaced := fs.allocInode("destination", "dir", 0o755, 1)
	replacedChild := fs.allocInode("destination/nested/file.txt", "file", 0o100644, 1)
	fs.dirHandles[1] = &DirHandle{inode: &InodeRef{Path: "source/nested"}}
	fs.fileHandles[2] = &FileHandle{path: "source/nested/file.txt"}
	fs.mu.Unlock()

	fs.moveInodePath("source", "destination")
	fs.moveOpenHandles("source", "destination")

	for id, want := range map[fuseops.InodeID]string{
		source.ID:  "destination",
		child.ID:   "destination/nested/file.txt",
		sibling.ID: "source-other/file.txt",
	} {
		got, err := fs.requireInode(id, syscall.ESTALE)
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != want {
			t.Fatalf("inode %d path = %q, want %q", id, got.Path, want)
		}
	}
	if _, err := fs.requireInode(replaced.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced inode error = %v, want ESTALE", err)
	}
	if _, err := fs.requireInode(replacedChild.ID, syscall.ESTALE); err != syscall.ESTALE {
		t.Fatalf("replaced child inode error = %v, want ESTALE", err)
	}
	if got := fs.pathToInode["destination/nested/file.txt"]; got != child.ID {
		t.Fatalf("destination child inode = %d, want source child %d", got, child.ID)
	}
	if err := fs.ForgetInode(context.Background(), &fuseops.ForgetInodeOp{Inode: replacedChild.ID, N: 1}); err != nil {
		t.Fatal(err)
	}
	if got := fs.pathToInode["destination/nested/file.txt"]; got != child.ID {
		t.Fatalf("forgetting replaced child removed source child mapping: got %d, want %d", got, child.ID)
	}
	if got := fs.dirHandles[1].inode.Path; got != "destination/nested" {
		t.Fatalf("directory handle path = %q", got)
	}
	if got := fs.fileHandles[2].path; got != "destination/nested/file.txt" {
		t.Fatalf("file handle path = %q", got)
	}
}

func TestRenameReturnsNativeErrorsBeforeMutatingOverlay(t *testing.T) {
	snapshot := &fakeSnapshot{nodes: map[string]model.BaseNode{
		"src-dir":       {Path: "src-dir", Type: "dir", Mode: 0o40000},
		"src-dir/child": {Path: "src-dir/child", Type: "dir", Mode: 0o40000},
		"dst-file":      {Path: "dst-file", Type: "file", Mode: 0o100644},
		"src-file":      {Path: "src-file", Type: "file", Mode: 0o100644},
		"dst-dir":       {Path: "dst-dir", Type: "dir", Mode: 0o40000},
	}}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := newResolver(snapshot, overlay)
	engine := &Engine{Resolver: resolver, Overlay: overlay}
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, resolver, engine)

	for _, test := range []struct {
		name string
		op   fuseops.RenameOp
		want error
	}{
		{
			name: "directory_over_file",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src-dir", NewParent: fuseops.RootInodeID, NewName: "dst-file"},
			want: syscall.ENOTDIR,
		},
		{
			name: "file_over_directory",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "src-file", NewParent: fuseops.RootInodeID, NewName: "dst-dir"},
			want: syscall.EISDIR,
		},
		{
			name: "missing_same_path",
			op:   fuseops.RenameOp{OldParent: fuseops.RootInodeID, OldName: "missing", NewParent: fuseops.RootInodeID, NewName: "missing"},
			want: syscall.ENOENT,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := fs.Rename(context.Background(), &test.op); err != test.want {
				t.Fatalf("Rename error = %v, want %v", err, test.want)
			}
		})
	}

	sourceLookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "src-dir"}
	if err := fs.LookUpInode(context.Background(), sourceLookup); err != nil {
		t.Fatal(err)
	}
	childLookup := &fuseops.LookUpInodeOp{Parent: sourceLookup.Entry.Child, Name: "child"}
	if err := fs.LookUpInode(context.Background(), childLookup); err != nil {
		t.Fatal(err)
	}
	selfDescendant := &fuseops.RenameOp{
		OldParent: fuseops.RootInodeID,
		OldName:   "src-dir",
		NewParent: childLookup.Entry.Child,
		NewName:   "moved",
	}
	if err := fs.Rename(context.Background(), selfDescendant); err != syscall.EINVAL {
		t.Fatalf("Rename into descendant error = %v, want EINVAL", err)
	}
	if len(overlay.entries) != 0 {
		t.Fatalf("rejected renames mutated overlay: %+v", overlay.entries)
	}
}

func TestInodeAndHandleCreationWaitForNamespaceMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(*ArtifactFuse) error
	}{
		{name: "lookup", call: func(fs *ArtifactFuse) error {
			return fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
		}},
		{name: "opendir", call: func(fs *ArtifactFuse) error {
			return fs.OpenDir(context.Background(), &fuseops.OpenDirOp{})
		}},
		{name: "readdirplus", call: func(fs *ArtifactFuse) error {
			return fs.ReadDirPlus(context.Background(), &fuseops.ReadDirPlusOp{})
		}},
		{name: "open", call: func(fs *ArtifactFuse) error {
			return fs.OpenFile(context.Background(), &fuseops.OpenFileOp{})
		}},
		{name: "setattr", call: func(fs *ArtifactFuse) error {
			return fs.SetInodeAttributes(context.Background(), &fuseops.SetInodeAttributesOp{})
		}},
		{name: "mkdir", call: func(fs *ArtifactFuse) error {
			return fs.MkDir(context.Background(), &fuseops.MkDirOp{})
		}},
		{name: "rmdir", call: func(fs *ArtifactFuse) error {
			return fs.RmDir(context.Background(), &fuseops.RmDirOp{})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs := &ArtifactFuse{}
			fs.handleOps.Lock()
			done := make(chan struct{})
			go func() {
				_ = test.call(fs)
				close(done)
			}()
			select {
			case <-done:
				t.Fatal("operation did not wait for namespace mutation")
			case <-time.After(25 * time.Millisecond):
			}
			fs.handleOps.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("operation remained blocked after namespace mutation")
			}
		})
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

func TestLookUpInodeResolvesUnknownBaseAttributeSizes(t *testing.T) {
	repo := model.RepoConfig{ID: "repo"}
	tests := []struct {
		name              string
		base              model.BaseNode
		overlay           map[string]model.OverlayEntry
		want              uint64
		wantHydrateCalls  int
		wantReadBlobCalls int
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
			name:              "base symlink",
			base:              model.BaseNode{RepoID: repo.ID, Path: "file.txt", Type: "symlink", Mode: 0o120000, ObjectOID: "blob", SizeState: "unknown"},
			want:              uint64(len("package.json")),
			wantReadBlobCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newResolver(
				&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": tt.base}},
				&fakeOverlay{entries: tt.overlay},
			)
			h := &fakeLookupHydrator{size: 12, data: []byte("package.json")}
			fs := NewArtifactFuse(repo, r, &Engine{Resolver: r, Repo: repo, Hydrator: h})
			op := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: "file.txt"}

			if err := fs.LookUpInode(context.Background(), op); err != nil {
				t.Fatalf("LookUpInode: %v", err)
			}
			if op.Entry.Attributes.Size != tt.want {
				t.Fatalf("lookup size = %d, want %d", op.Entry.Attributes.Size, tt.want)
			}
			if h.calls != tt.wantHydrateCalls {
				t.Fatalf("EnsureHydrated calls = %d, want %d", h.calls, tt.wantHydrateCalls)
			}
			if h.readBlobCalls != tt.wantReadBlobCalls {
				t.Fatalf("ReadBlob calls = %d, want %d", h.readBlobCalls, tt.wantReadBlobCalls)
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
	size          int64
	calls         int
	readBlobCalls int
	err           error
	path          string
	data          []byte
}

func (f *fakeLookupHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeLookupHydrator) EnqueueBatch([]model.HydrationTask) {}

func (f *fakeLookupHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, _ model.BaseNode) (string, int64, error) {
	f.calls++
	if f.err != nil {
		return "", 0, f.err
	}
	return f.path, f.size, nil
}

func (f *fakeLookupHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := f.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (f *fakeLookupHydrator) ReadBlob(_ context.Context, _ model.RepoConfig, _ model.BaseNode, _ int64) ([]byte, error) {
	f.readBlobCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func (f *fakeLookupHydrator) QueueDepth(model.RepoID) int { return 0 }
