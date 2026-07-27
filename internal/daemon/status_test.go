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

func TestVerifiedSourceStatusAndRefreshPolicy(t *testing.T) {
	const requiredCommit = "0123456789012345678901234567890123456789"
	cfg := model.RepoConfig{
		Branch:                "refs/heads/main",
		RequiredCommit:        requiredCommit,
		AcquiredRef:           "refs/heads/main",
		AcquiredCommit:        requiredCommit,
		AcquiredAt:            time.Now(),
		RemoteRefreshDisabled: true,
	}
	st := model.RepoRuntimeState{CurrentHEADOID: requiredCommit}
	applySourceStatus(&st, cfg)
	if st.SourceRef != cfg.Branch || st.RequiredCommit != requiredCommit || st.Acquisition != "verified" || !st.RemoteRefreshDisabled {
		t.Fatalf("verified source status = %+v", st)
	}

	// Acquisition is historical evidence, not a mutable comparison against HEAD.
	st.CurrentHEADOID = strings.Repeat("f", 40)
	applySourceStatus(&st, cfg)
	if st.Acquisition != "verified" {
		t.Fatalf("acquisition = %q, want verified", st.Acquisition)
	}

	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg.ID = "verified"
	cfg.Name = "verified"
	cfg.PrepareState = model.PrepareStateReady
	cfg.Enabled = true
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := svc.FetchNow(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "refresh is disabled") {
		t.Fatalf("FetchNow error = %v, want refresh-disabled rejection", err)
	}
}

func TestRequiredCommitIgnoresHEADWatcher(t *testing.T) {
	const requiredCommit = "0123456789012345678901234567890123456789"
	runtime := &repoRuntime{
		cfg:   model.RepoConfig{ID: "verified", RequiredCommit: requiredCommit, GitDir: filepath.Join(t.TempDir(), "missing")},
		state: model.RepoRuntimeState{CurrentHEADOID: requiredCommit, SnapshotGeneration: 1},
	}
	service := &Service{running: map[model.RepoID]*repoRuntime{"verified": runtime}}
	service.onHEADChanged(context.Background(), runtime)
	if runtime.state.CurrentHEADOID != requiredCommit || runtime.state.SnapshotGeneration != 1 {
		t.Fatalf("fixed base changed: %+v", runtime.state)
	}
}

func TestPersistedVerifiedStatusDoesNotCountAcquisitionAsRefresh(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(gitDir, "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("acquisition\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acquiredAt := time.Now()
	acquisitionFetchAt := acquiredAt.Add(-time.Minute)
	if err := os.Chtimes(fetchHead, acquisitionFetchAt, acquisitionFetchAt); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{
		ID:             "verified",
		GitDir:         gitDir,
		Branch:         "refs/heads/main",
		RequiredCommit: strings.Repeat("a", 40),
		AcquiredRef:    "refs/heads/main",
		AcquiredCommit: strings.Repeat("a", 40),
		AcquiredAt:     acquiredAt,
	}
	svc := &Service{}
	st := svc.readPersistedStatus(context.Background(), cfg)
	if !st.LastFetchAt.IsZero() || st.LastFetchResult != "never" {
		t.Fatalf("acquisition reported as remote refresh: %+v", st)
	}

	refreshAt := acquiredAt.Add(time.Minute)
	if err := os.Chtimes(fetchHead, refreshAt, refreshAt); err != nil {
		t.Fatal(err)
	}
	st = svc.readPersistedStatus(context.Background(), cfg)
	if !st.LastFetchAt.Equal(refreshAt) || st.LastFetchResult != "ok" {
		t.Fatalf("later remote refresh status = %+v, want %v and ok", st, refreshAt)
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
		{Path: "readme-link", Kind: model.OverlayKindSymlink, TargetPath: "../README.md"},
		{Path: ".", Kind: model.OverlayKindMkdir},
	})
	want := []string{
		"new/name.go",
		"old/name.go",
		"readme-link",
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
