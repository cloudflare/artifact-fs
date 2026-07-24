package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

func TestSnapshotModeStatusAndFetchPolicy(t *testing.T) {
	const expectedOID = "0123456789012345678901234567890123456789"
	st := model.RepoRuntimeState{CurrentHEADOID: expectedOID}
	cfg := model.RepoConfig{Mode: model.RepoModeSnapshot, ExpectedOID: expectedOID}
	applyRepoModeStatus(&st, cfg)
	if st.Mode != model.RepoModeSnapshot || st.ExpectedOID != expectedOID || !st.TargetVerified {
		t.Fatalf("snapshot status = %+v", st)
	}
	st.CurrentHEADOID = strings.Repeat("f", 40)
	applyRepoModeStatus(&st, cfg)
	if st.TargetVerified {
		t.Fatal("mismatched snapshot reported target_verified=true")
	}

	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg.ID = "snapshot"
	cfg.Name = "snapshot"
	cfg.Branch = "main"
	cfg.PrepareState = model.PrepareStateReady
	cfg.Enabled = true
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.FetchNow(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("FetchNow error = %v, want pinned snapshot rejection", err)
	}
}

func TestReadPersistedStatusIncludesHydrationStats(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	gitDir := filepath.Join(root, "git")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, strings.Repeat("a", 40)), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write blob-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, strings.Repeat("b", 64)), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write blob-b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, ".artifact-fs-blob-partial"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &Service{}
	st := svc.readPersistedStatus(context.Background(), model.RepoConfig{ID: "repo", BlobCacheDir: cacheDir, GitDir: gitDir})

	if st.LastFetchResult != "never" {
		t.Fatalf("LastFetchResult = %q, want never", st.LastFetchResult)
	}
	if !st.LastFetchAt.IsZero() {
		t.Fatalf("LastFetchAt = %v, want zero", st.LastFetchAt)
	}
	if st.HydratedBlobCount != 2 {
		t.Fatalf("HydratedBlobCount = %d, want 2", st.HydratedBlobCount)
	}
	if st.HydratedBlobBytes != 8 {
		t.Fatalf("HydratedBlobBytes = %d, want 8", st.HydratedBlobBytes)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestWriteHookFieldRejectsShortWrite(t *testing.T) {
	if err := writeHookField(shortWriter{}, "token"); err != io.ErrShortWrite {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
}

type blockingMountedFS struct {
	started    chan context.Context
	release    chan struct{}
	unmountErr error
}

func (m *blockingMountedFS) Join(ctx context.Context) error {
	m.started <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.release:
		return nil
	}
}

func (m *blockingMountedFS) Unmount() error { return m.unmountErr }

func TestJoinRuntimeMountIsNotCanceledWithWorkers(t *testing.T) {
	runtimeCtx, cancel := context.WithCancel(context.Background())
	mfs := &blockingMountedFS{started: make(chan context.Context, 1), release: make(chan struct{})}
	rt := &repoRuntime{ctx: runtimeCtx, mfs: mfs, stopping: true}
	svc := &Service{}
	svc.joinRuntimeMount(rt)
	joinCtx := <-mfs.started
	cancel()
	select {
	case <-joinCtx.Done():
		t.Fatal("Join context was canceled with runtime workers")
	case <-time.After(25 * time.Millisecond):
	}
	close(mfs.release)
	select {
	case <-rt.joinDone:
	case <-time.After(time.Second):
		t.Fatal("Join did not finish after mount drained")
	}
}

func TestStopRuntimeKeepsWorkersAliveWhenUnmountFails(t *testing.T) {
	runtimeCtx, cancel := context.WithCancel(context.Background())
	rt := &repoRuntime{
		ctx:    runtimeCtx,
		cancel: cancel,
		mfs:    &blockingMountedFS{unmountErr: errors.New("busy")},
		cfg:    model.RepoConfig{Name: "repo"},
	}
	if err := (&Service{}).stopRuntime(rt); err == nil {
		t.Fatal("expected unmount failure")
	}
	if err := runtimeCtx.Err(); err != nil {
		t.Fatalf("runtime context canceled after failed unmount: %v", err)
	}
}

func TestFSMonitorDirtyPathsIncludesChangedPathsAndRenameEndpoints(t *testing.T) {
	paths := fsMonitorDirtyPaths([]model.OverlayEntry{
		{Path: "src/pkg/file.go", Kind: model.OverlayKindModify},
		{Path: "new/name.go", Kind: model.OverlayKindRename, TargetPath: "old/name.go"},
		{Path: ".", Kind: model.OverlayKindMkdir},
	})
	want := []string{
		"new/name.go",
		"old/name.go",
		"src/pkg/file.go",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("dirty paths = %#v, want %#v", paths, want)
	}
}

func TestFSMonitorHookOutputsDirtyOverlayPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "main",
		RefreshInterval: time.Minute,
		Enabled:         true,
	}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ov.CreateFile(ctx, "new/file.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ov.Remove(ctx, "deleted.txt"); err != nil {
		t.Fatal(err)
	}
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := svc.FSMonitorHook(ctx, "repo", &out); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out.String(), "\x00")
	if !strings.HasPrefix(parts[0], "artifact-fs:repo:") {
		t.Fatalf("token = %q", parts[0])
	}
	got := map[string]bool{}
	for _, p := range parts[1:] {
		if p != "" {
			got[p] = true
		}
	}
	for _, want := range []string{"deleted.txt", "new/file.txt"} {
		if !got[want] {
			t.Fatalf("fsmonitor paths missing %q: %v", want, got)
		}
	}
}
