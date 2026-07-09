package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type failingMountedFS struct {
	err error
}

func (f *failingMountedFS) Join(context.Context) error { return nil }
func (f *failingMountedFS) Unmount() error             { return f.err }

func TestUnmountFailureRetainsLiveRuntime(t *testing.T) {
	want := errors.New("unmount failed")
	runtimeCtx, cancel := context.WithCancel(context.Background())
	id := model.RepoID("repo")
	rt := &repoRuntime{ctx: runtimeCtx, cancel: cancel, mfs: &failingMountedFS{err: want}}
	s := &Service{running: map[model.RepoID]*repoRuntime{id: rt}}

	if err := s.unmount(id); !errors.Is(err, want) {
		t.Fatalf("unmount error = %v, want %v", err, want)
	}
	if s.running[id] != rt {
		t.Fatal("failed unmount removed runtime")
	}
	if rt.stopping {
		t.Fatal("failed unmount left runtime marked stopping")
	}
	select {
	case <-runtimeCtx.Done():
		t.Fatal("failed unmount canceled live runtime")
	default:
	}
}
