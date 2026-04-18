//go:build !windows

package fusefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
