//go:build !windows

package fusefs

import (
	"context"
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
