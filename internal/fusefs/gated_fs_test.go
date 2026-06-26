//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type recordingFS struct {
	fuseutil.NotImplementedFileSystem
	lookups atomic.Int32
}

func (fs *recordingFS) LookUpInode(context.Context, *fuseops.LookUpInodeOp) error {
	fs.lookups.Add(1)
	return nil
}

func TestGatedFileSystemBlocksUntilReady(t *testing.T) {
	next := &recordingFS{}
	gate := NewReadyGate(false)
	fs := NewGatedFileSystem(next, gate)

	done := make(chan error, 1)
	go func() {
		done <- fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
	}()

	select {
	case err := <-done:
		t.Fatalf("LookUpInode returned before ready: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := next.lookups.Load(); got != 0 {
		t.Fatalf("lookups before ready = %d, want 0", got)
	}

	gate.MarkReady()
	if err := <-done; err != nil {
		t.Fatalf("LookUpInode after ready: %v", err)
	}
	if got := next.lookups.Load(); got != 1 {
		t.Fatalf("lookups after ready = %d, want 1", got)
	}
}

func TestGatedFileSystemFailedGateReturnsEIO(t *testing.T) {
	next := &recordingFS{}
	gate := NewReadyGate(false)
	gate.MarkFailed(errors.New("clone failed"))
	fs := NewGatedFileSystem(next, gate)

	err := fs.LookUpInode(context.Background(), &fuseops.LookUpInodeOp{})
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("LookUpInode error = %v, want EIO", err)
	}
	if got := next.lookups.Load(); got != 0 {
		t.Fatalf("lookups after failed gate = %d, want 0", got)
	}
}
