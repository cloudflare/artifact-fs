//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

type fakeSymlinkHydrator struct {
	calls         int
	readBlobCalls int
	cachePath     string
	size          int64
	err           error
	readBlobData  []byte
	readBlobErr   error
}

func (f *fakeSymlinkHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeSymlinkHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, _ model.BaseNode) (string, int64, error) {
	f.calls++
	return f.cachePath, f.size, f.err
}

func (f *fakeSymlinkHydrator) ReadBlob(_ context.Context, _ model.RepoConfig, _ model.BaseNode, _ int64) ([]byte, error) {
	f.readBlobCalls++
	return f.readBlobData, f.readBlobErr
}

func (f *fakeSymlinkHydrator) QueueDepth(model.RepoID) int { return 0 }

func TestReadSymlinkRejectsKnownOversizedBlobBeforeHydration(t *testing.T) {
	hydrator := &fakeSymlinkHydrator{}
	repoID := model.RepoID("repo")
	resolver := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"link": {
				RepoID:    repoID,
				Path:      "link",
				Type:      "symlink",
				Mode:      0o120000,
				ObjectOID: "blob",
				SizeState: "known",
				SizeBytes: int64(maxSymlinkTargetBytes + 1),
			},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(model.RepoConfig{ID: repoID}, resolver, &Engine{Hydrator: hydrator})

	fs.mu.Lock()
	ref := fs.allocInode("link", "symlink", 0o120000)
	fs.mu.Unlock()

	op := &fuseops.ReadSymlinkOp{Inode: ref.ID}
	err := fs.ReadSymlink(context.Background(), op)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("err = %v, want ENAMETOOLONG", err)
	}
	if hydrator.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", hydrator.calls)
	}
	if hydrator.readBlobCalls != 0 {
		t.Fatalf("ReadBlob calls = %d, want 0", hydrator.readBlobCalls)
	}
	if op.Target != "" {
		t.Fatalf("target = %q, want empty", op.Target)
	}
}

func TestReadSymlinkRejectsNegativeKnownBlobBeforeHydration(t *testing.T) {
	hydrator := &fakeSymlinkHydrator{}
	repoID := model.RepoID("repo")
	resolver := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"link": {
				RepoID:    repoID,
				Path:      "link",
				Type:      "symlink",
				Mode:      0o120000,
				ObjectOID: "blob",
				SizeState: "known",
				SizeBytes: -1,
			},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(model.RepoConfig{ID: repoID}, resolver, &Engine{Hydrator: hydrator})

	fs.mu.Lock()
	ref := fs.allocInode("link", "symlink", 0o120000)
	fs.mu.Unlock()

	op := &fuseops.ReadSymlinkOp{Inode: ref.ID}
	err := fs.ReadSymlink(context.Background(), op)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v, want EIO", err)
	}
	if hydrator.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", hydrator.calls)
	}
	if hydrator.readBlobCalls != 0 {
		t.Fatalf("ReadBlob calls = %d, want 0", hydrator.readBlobCalls)
	}
	if op.Target != "" {
		t.Fatalf("target = %q, want empty", op.Target)
	}
}

func TestReadSymlinkRejectsUnknownOversizedBlobWithoutHydration(t *testing.T) {
	hydrator := &fakeSymlinkHydrator{readBlobErr: model.ErrBlobTooLarge}
	repoID := model.RepoID("repo")
	resolver := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"link": {
				RepoID:    repoID,
				Path:      "link",
				Type:      "symlink",
				Mode:      0o120000,
				ObjectOID: "blob",
				SizeState: "unknown",
			},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(model.RepoConfig{ID: repoID}, resolver, &Engine{Hydrator: hydrator})

	fs.mu.Lock()
	ref := fs.allocInode("link", "symlink", 0o120000)
	fs.mu.Unlock()

	op := &fuseops.ReadSymlinkOp{Inode: ref.ID}
	err := fs.ReadSymlink(context.Background(), op)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("err = %v, want ENAMETOOLONG", err)
	}
	if hydrator.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", hydrator.calls)
	}
	if hydrator.readBlobCalls != 1 {
		t.Fatalf("ReadBlob calls = %d, want 1", hydrator.readBlobCalls)
	}
	if op.Target != "" {
		t.Fatalf("target = %q, want empty", op.Target)
	}
}

func TestReadSymlinkReadsUnknownBlobThroughBoundedRead(t *testing.T) {
	hydrator := &fakeSymlinkHydrator{readBlobData: []byte("../target")}
	repoID := model.RepoID("repo")
	resolver := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{
			"link": {
				RepoID:    repoID,
				Path:      "link",
				Type:      "symlink",
				Mode:      0o120000,
				ObjectOID: "blob",
				SizeState: "unknown",
			},
		}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	fs := NewArtifactFuse(model.RepoConfig{ID: repoID}, resolver, &Engine{Hydrator: hydrator})

	fs.mu.Lock()
	ref := fs.allocInode("link", "symlink", 0o120000)
	fs.mu.Unlock()

	op := &fuseops.ReadSymlinkOp{Inode: ref.ID}
	err := fs.ReadSymlink(context.Background(), op)
	if err != nil {
		t.Fatalf("ReadSymlink: %v", err)
	}
	if hydrator.calls != 0 {
		t.Fatalf("EnsureHydrated calls = %d, want 0", hydrator.calls)
	}
	if hydrator.readBlobCalls != 1 {
		t.Fatalf("ReadBlob calls = %d, want 1", hydrator.readBlobCalls)
	}
	if op.Target != "../target" {
		t.Fatalf("target = %q, want ../target", op.Target)
	}
}
