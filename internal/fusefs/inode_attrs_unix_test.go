//go:build !windows

package fusefs

import (
	"math"
	"os"
	"testing"
	"time"
)

func TestInodeAttrs_ClampsNegativeSizeToZero(t *testing.T) {
	cases := []struct {
		name string
		size int64
	}{
		{"minus one", -1},
		{"min int64", math.MinInt64},
		{"arbitrary negative", -4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inodeAttrs(0o644, tc.size, "file", time.Unix(0, 0))
			if got.Size != 0 {
				t.Fatalf("file size = %d, want 0 for input %d", got.Size, tc.size)
			}
		})
	}
}

func TestInodeAttrs_PreservesPositiveFileSize(t *testing.T) {
	got := inodeAttrs(0o644, 42, "file", time.Unix(0, 0))
	if got.Size != 42 {
		t.Fatalf("file size = %d, want 42", got.Size)
	}
}

func TestInodeAttrs_PreservesMaxInt64(t *testing.T) {
	got := inodeAttrs(0o644, math.MaxInt64, "file", time.Unix(0, 0))
	if got.Size != math.MaxInt64 {
		t.Fatalf("file size = %d, want MaxInt64", got.Size)
	}
}

func TestInodeAttrs_DirZeroSizeBecomes4096(t *testing.T) {
	got := inodeAttrs(0o755, 0, "dir", time.Unix(0, 0))
	if got.Size != 4096 {
		t.Fatalf("dir size = %d, want 4096", got.Size)
	}
}

func TestInodeAttrs_DirNegativeSizeAlsoBecomes4096(t *testing.T) {
	// Negative clamps to 0, which the dir branch then upgrades to 4096.
	got := inodeAttrs(0o755, -1, "dir", time.Unix(0, 0))
	if got.Size != 4096 {
		t.Fatalf("dir size = %d, want 4096", got.Size)
	}
}

func TestInodeAttrs_SymlinkModeBitSet(t *testing.T) {
	got := inodeAttrs(0o777, 16, "symlink", time.Unix(0, 0))
	if got.Mode&os.ModeSymlink == 0 {
		t.Fatalf("symlink mode bit not set in %v", got.Mode)
	}
	if got.Size != 16 {
		t.Fatalf("symlink size = %d, want 16", got.Size)
	}
}

func TestInodeAttrs_DefaultFileModeWhenZero(t *testing.T) {
	got := inodeAttrs(0, 1, "file", time.Unix(0, 0))
	if got.Mode.Perm() != 0o644 {
		t.Fatalf("file default perm = %v, want 0644", got.Mode.Perm())
	}
}

func TestInodeAttrs_DefaultDirModeWhenZero(t *testing.T) {
	got := inodeAttrs(0, 1, "dir", time.Unix(0, 0))
	if got.Mode.Perm() != 0o755 {
		t.Fatalf("dir default perm = %v, want 0755", got.Mode.Perm())
	}
	if got.Mode&os.ModeDir == 0 {
		t.Fatalf("dir mode bit not set in %v", got.Mode)
	}
}
