package daemon

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type failingMountedFS struct {
	err error
}

type blockingMountedFS struct {
	joinStarted chan struct{}
	unmounted   chan struct{}
	releaseJoin chan struct{}
}

type errorMountedFS struct {
	err error
}

func (f *errorMountedFS) Join(context.Context) error { return f.err }
func (*errorMountedFS) Unmount() error               { return nil }

type requestDrainingMountedFS struct {
	requestDone <-chan struct{}
}

func (f *requestDrainingMountedFS) Join(context.Context) error {
	<-f.requestDone
	return nil
}

func (*requestDrainingMountedFS) Unmount() error { return nil }

func (f *blockingMountedFS) Join(ctx context.Context) error {
	close(f.joinStarted)
	select {
	case <-f.releaseJoin:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *blockingMountedFS) Unmount() error {
	close(f.unmounted)
	return nil
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

func TestStopRuntimeWaitsForMountJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mounted := &blockingMountedFS{
			joinStarted: make(chan struct{}),
			unmounted:   make(chan struct{}),
			releaseJoin: make(chan struct{}),
		}
		runtimeCtx, cancel := context.WithCancel(context.Background())
		rt := &repoRuntime{ctx: runtimeCtx, cancel: cancel, mfs: mounted}
		s := &Service{running: map[model.RepoID]*repoRuntime{}}
		s.startRuntime(rt)
		<-mounted.joinStarted
		cancel()
		done := make(chan error, 1)
		go func() { done <- s.stopRuntime(rt) }()
		<-mounted.unmounted
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("stopRuntime returned before Join drained: %v", err)
		default:
		}
		close(mounted.releaseJoin)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestUnmountRemovesRuntimeAfterJoinError(t *testing.T) {
	want := errors.New("serve failed")
	runtimeCtx, cancel := context.WithCancel(context.Background())
	id := model.RepoID("repo")
	rt := &repoRuntime{cfg: model.RepoConfig{ID: id}, ctx: runtimeCtx, cancel: cancel, mfs: &errorMountedFS{err: want}}
	s := &Service{running: map[model.RepoID]*repoRuntime{}}
	s.startRuntime(rt)
	if err := s.unmount(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.running[id]; ok {
		t.Fatal("runtime remained registered after successful unmount")
	}
}

func TestStopRuntimeFailsGateBeforeWaitingForJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := fusefs.NewReadyGate(false)
		requestDone := make(chan struct{})
		go func() {
			_ = gate.Wait(context.Background())
			close(requestDone)
		}()
		runtimeCtx, cancel := context.WithCancel(context.Background())
		rt := &repoRuntime{
			ctx: runtimeCtx, cancel: cancel, gate: gate,
			mfs: &requestDrainingMountedFS{requestDone: requestDone},
		}
		s := &Service{running: map[model.RepoID]*repoRuntime{}}
		s.startRuntime(rt)
		synctest.Wait()
		done := make(chan error, 1)
		go func() { done <- s.stopRuntime(rt) }()
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
