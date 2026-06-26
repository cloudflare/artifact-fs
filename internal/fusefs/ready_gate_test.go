package fusefs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReadyGateWaitsUntilReady(t *testing.T) {
	g := NewReadyGate(false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- g.Wait(ctx)
	}()

	select {
	case err := <-done:
		t.Fatalf("Wait returned before ready: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	g.MarkReady()
	if err := <-done; err != nil {
		t.Fatalf("Wait returned error after ready: %v", err)
	}
}

func TestReadyGateFailureFailsFastAndCanReset(t *testing.T) {
	g := NewReadyGate(false)
	g.MarkFailed(errors.New("clone failed"))

	if err := g.Wait(context.Background()); err == nil || err.Error() != "clone failed" {
		t.Fatalf("Wait error = %v, want clone failed", err)
	}

	g.Reset()
	g.MarkReady()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after reset/ready: %v", err)
	}
}

func TestReadyGateMarkReadyAfterFailureDoesNotPanic(t *testing.T) {
	g := NewReadyGate(false)
	g.MarkFailed(errors.New("clone failed"))
	g.MarkReady()
}
