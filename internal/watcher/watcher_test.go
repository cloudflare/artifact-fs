package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchTriggersOnHeadChange(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	mainRef := filepath.Join(gitDir, "refs", "heads", "main")
	featureRef := filepath.Join(gitDir, "refs", "heads", "feature")
	if err := os.MkdirAll(filepath.Dir(mainRef), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(mainRef, []byte("main"), 0o644); err != nil {
		t.Fatalf("write main ref: %v", err)
	}
	if err := os.WriteFile(featureRef, []byte("feature"), 0o644); err != nil {
		t.Fatalf("write feature ref: %v", err)
	}

	p := New(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 1)
	go p.Watch(ctx, gitDir, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})

	// Prime the previous mtime snapshot before changing HEAD.
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatalf("update HEAD: %v", err)
	}

	select {
	case <-changed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected HEAD change notification")
	}
}

func TestWatchTriggersOnCurrentBranchAdvance(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	refPath := filepath.Join(gitDir, "refs", "heads", "main")
	if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(refPath, []byte("commit-1"), 0o644); err != nil {
		t.Fatalf("write ref: %v", err)
	}

	p := New(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 1)
	go p.Watch(ctx, gitDir, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})

	// Prime the previous mtime snapshots before advancing the current branch ref.
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(refPath, []byte("commit-2"), 0o644); err != nil {
		t.Fatalf("update ref: %v", err)
	}

	select {
	case <-changed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected branch advance notification")
	}
}

func TestHeadChangedPrimesNewBranchRef(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	mainRef := filepath.Join(gitDir, "refs", "heads", "main")
	featureRef := filepath.Join(gitDir, "refs", "heads", "feature")
	if err := os.MkdirAll(filepath.Dir(mainRef), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(mainRef, []byte("commit-1"), 0o644); err != nil {
		t.Fatalf("write main ref: %v", err)
	}
	if err := os.WriteFile(featureRef, []byte("commit-a"), 0o644); err != nil {
		t.Fatalf("write feature ref: %v", err)
	}

	p := New(5 * time.Millisecond)
	if p.headChanged(headPath) {
		t.Fatal("initial poll should only prime state")
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatalf("switch HEAD: %v", err)
	}
	if !p.headChanged(headPath) {
		t.Fatal("expected HEAD switch to be detected")
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(featureRef, []byte("commit-b"), 0o644); err != nil {
		t.Fatalf("advance feature ref: %v", err)
	}
	if !p.headChanged(headPath) {
		t.Fatal("expected first advance on new branch to be detected")
	}
}

func TestHeadChangedTreatsFirstLooseRefAppearanceAsChange(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	mainRef := filepath.Join(gitDir, "refs", "heads", "main")
	featureRef := filepath.Join(gitDir, "refs", "heads", "feature")
	if err := os.MkdirAll(filepath.Dir(mainRef), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(mainRef, []byte("commit-1"), 0o644); err != nil {
		t.Fatalf("write main ref: %v", err)
	}

	p := New(5 * time.Millisecond)
	if p.headChanged(headPath) {
		t.Fatal("initial poll should only prime state")
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatalf("switch HEAD: %v", err)
	}
	if !p.headChanged(headPath) {
		t.Fatal("expected HEAD switch to be detected")
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(featureRef, []byte("commit-a"), 0o644); err != nil {
		t.Fatalf("create feature ref: %v", err)
	}
	if !p.headChanged(headPath) {
		t.Fatal("expected first loose ref appearance to be treated as a change")
	}
}

func TestHeadChangedTreatsPackedStartupRefAppearanceAsChange(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	refPath := filepath.Join(gitDir, "refs", "heads", "main")
	if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	p := New(5 * time.Millisecond)
	if p.headChanged(headPath) {
		t.Fatal("initial poll should only prime state")
	}

	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(refPath, []byte("commit-1"), 0o644); err != nil {
		t.Fatalf("create main ref: %v", err)
	}
	if !p.headChanged(headPath) {
		t.Fatal("expected first loose ref after startup to be treated as a change")
	}
}

func TestWatchIgnoresIndexOnlyChanges(t *testing.T) {
	gitDir := t.TempDir()
	headPath := filepath.Join(gitDir, "HEAD")
	indexPath := filepath.Join(gitDir, "index")
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.WriteFile(indexPath, []byte("index"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	p := New(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 1)
	go p.Watch(ctx, gitDir, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})

	// Prime the previous mtime snapshot before changing only the index.
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(indexPath, []byte("index updated"), 0o644); err != nil {
		t.Fatalf("update index: %v", err)
	}

	select {
	case <-changed:
		t.Fatal("unexpected callback for index-only change")
	case <-time.After(100 * time.Millisecond):
	}
}
