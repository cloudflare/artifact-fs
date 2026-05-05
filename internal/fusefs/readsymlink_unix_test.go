//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/jacobsa/fuse/fuseops"
)

type fakeSymlinkHydrator struct {
	calls     int
	cachePath string
	size      int64
	err       error
}

func (f *fakeSymlinkHydrator) Enqueue(model.HydrationTask) {}

func (f *fakeSymlinkHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, _ model.BaseNode) (string, int64, error) {
	f.calls++
	return f.cachePath, f.size, f.err
}

func (f *fakeSymlinkHydrator) QueueDepth(model.RepoID) int { return 0 }

func writeBlob(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestReadSymlinkTarget_EmptyTarget(t *testing.T) {
	dir := t.TempDir()
	p := writeBlob(t, dir, "empty", nil)
	got, err := readSymlinkTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("target = %q, want empty string", got)
	}
}

func TestReadSymlinkTarget_ShortTarget(t *testing.T) {
	dir := t.TempDir()
	p := writeBlob(t, dir, "short", []byte("../relative/path"))
	got, err := readSymlinkTarget(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "../relative/path" {
		t.Fatalf("target = %q, want %q", got, "../relative/path")
	}
}

func TestReadSymlinkTarget_AtLimit(t *testing.T) {
	dir := t.TempDir()
	data := []byte(strings.Repeat("a", maxSymlinkTargetBytes))
	p := writeBlob(t, dir, "at-limit", data)
	got, err := readSymlinkTarget(p)
	if err != nil {
		t.Fatalf("unexpected error at %d bytes: %v", maxSymlinkTargetBytes, err)
	}
	if len(got) != maxSymlinkTargetBytes {
		t.Fatalf("target length = %d, want %d", len(got), maxSymlinkTargetBytes)
	}
}

func TestReadSymlinkTarget_OverLimit(t *testing.T) {
	dir := t.TempDir()
	data := []byte(strings.Repeat("a", maxSymlinkTargetBytes+1))
	p := writeBlob(t, dir, "over-limit", data)
	_, err := readSymlinkTarget(p)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("err = %v, want ENAMETOOLONG", err)
	}
}

func TestReadSymlinkTarget_FarOverLimit(t *testing.T) {
	// A blob that's orders of magnitude past PATH_MAX should still be read
	// into a bounded slice and rejected, not slurped whole.
	dir := t.TempDir()
	data := make([]byte, 1<<20) // 1 MiB
	for i := range data {
		data[i] = 'x'
	}
	p := writeBlob(t, dir, "huge", data)
	_, err := readSymlinkTarget(p)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("err = %v, want ENAMETOOLONG", err)
	}
}

func TestReadSymlinkTarget_MissingFile(t *testing.T) {
	_, err := readSymlinkTarget(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing cache file, got nil")
	}
	if errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("err = %v, want non-ENAMETOOLONG for missing file", err)
	}
}

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
	if op.Target != "" {
		t.Fatalf("target = %q, want empty", op.Target)
	}
}
