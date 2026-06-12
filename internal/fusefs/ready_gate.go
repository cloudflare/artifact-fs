package fusefs

import (
	"context"
	"errors"
	"sync"
)

var ErrRepoNotReady = errors.New("repo is not ready")

type ReadyGate struct {
	mu     sync.Mutex
	ready  bool
	err    error
	done   chan struct{}
	closed bool
}

func NewReadyGate(ready bool) *ReadyGate {
	g := &ReadyGate{ready: ready, done: make(chan struct{})}
	if ready {
		close(g.done)
		g.closed = true
	}
	return g
}

func (g *ReadyGate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.ready {
		g.mu.Unlock()
		return nil
	}
	if g.err != nil {
		err := g.err
		g.mu.Unlock()
		return err
	}
	done := g.done
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ready {
		return nil
	}
	if g.err != nil {
		return g.err
	}
	return ErrRepoNotReady
}

func (g *ReadyGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ready && g.err == nil {
		return
	}
	g.ready = false
	g.err = nil
	g.done = make(chan struct{})
	g.closed = false
}

func (g *ReadyGate) MarkReady() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ready {
		return
	}
	g.ready = true
	g.err = nil
	if !g.closed {
		close(g.done)
		g.closed = true
	}
}

func (g *ReadyGate) MarkFailed(err error) {
	if g == nil {
		return
	}
	if err == nil {
		err = ErrRepoNotReady
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		g.err = err
		return
	}
	if g.ready {
		return
	}
	g.err = err
	if !g.closed {
		close(g.done)
		g.closed = true
	}
}
