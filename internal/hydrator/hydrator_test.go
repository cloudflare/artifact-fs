package hydrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestClassifyPriority(t *testing.T) {
	if got := ClassifyPriority("README.md"); got < PriorityBootstrap {
		t.Fatalf("README should be boosted, got %d", got)
	}
	if got := ClassifyPriority("src/main.go"); got < PriorityLikelyText {
		t.Fatalf("go file should be likely text, got %d", got)
	}
	if got := ClassifyPriority("assets/logo.png"); got > PriorityBinary {
		t.Fatalf("png should be penalized, got %d", got)
	}
}

func TestEnsureHydratedRefetchesTruncatedKnownBlob(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: 7}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("content")}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(fetcher.payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(fetcher.payload))
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(fetcher.payload) {
		t.Fatalf("cache contents = %q, want %q", data, fetcher.payload)
	}
}

func TestEnsureHydratedUsesValidKnownBlobCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "known", SizeBytes: int64(len(payload))}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("new-data"), verifyOK: true}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(payload))
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestEnsureHydratedUsesValidUnknownSizeCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("new-data"), verifyOK: true}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotPath, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotPath != cachePath {
		t.Fatalf("cache path = %q, want %q", gotPath, cachePath)
	}
	if gotSize != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(payload))
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestEnsureHydratedRefetchesUnknownSizeCacheHit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: []byte("content"), verifyOK: false}
	h := New(fetcher)
	h.Start(1, cfg)
	defer h.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, gotSize, err := h.EnsureHydrated(ctx, cfg, node)
	if err != nil {
		t.Fatalf("EnsureHydrated: %v", err)
	}
	if gotSize != int64(len(fetcher.payload)) {
		t.Fatalf("size = %d, want %d", gotSize, len(fetcher.payload))
	}
	if fetcher.Calls() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.Calls())
	}
}

func TestEnsureHydratedVerifiesUnknownCacheHitOnce(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	releaseVerify := make(chan struct{})
	verifyStarted := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true, verifyStarted: verifyStarted, verifyWait: releaseVerify}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const readers = 8
	errCh := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := h.EnsureHydrated(ctx, cfg, node)
			errCh <- err
		}()
	}
	<-verifyStarted
	runtime.Gosched()
	close(releaseVerify)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("EnsureHydrated: %v", err)
		}
	}
	if fetcher.VerifyCalls() != 1 {
		t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
	}
	if fetcher.Calls() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetcher.Calls())
	}
}

func TestEnsureHydratedVerificationIgnoresLeaderTimeout(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	releaseVerify := make(chan struct{})
	verifyStarted := make(chan struct{})
	fetcher := &fakeBlobFetcher{payload: payload, verifyOK: true, verifyStarted: verifyStarted, verifyWait: releaseVerify}
	h := New(fetcher)

	leaderCtx, leaderCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer leaderCancel()
	leaderErrCh := make(chan error, 1)
	go func() {
		_, _, err := h.EnsureHydrated(leaderCtx, cfg, node)
		leaderErrCh <- err
	}()
	<-verifyStarted

	followerCtx, followerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer followerCancel()
	followerErrCh := make(chan error, 1)
	go func() {
		_, _, err := h.EnsureHydrated(followerCtx, cfg, node)
		followerErrCh <- err
	}()

	leaderErr := <-leaderErrCh
	if leaderErr == nil {
		t.Fatal("leader call unexpectedly succeeded")
	}
	close(releaseVerify)
	if err := <-followerErrCh; err != nil {
		t.Fatalf("follower EnsureHydrated: %v", err)
	}
	if fetcher.VerifyCalls() != 1 {
		t.Fatalf("verify calls = %d, want 1", fetcher.VerifyCalls())
	}
}

func TestValidateCachedBlobKeepsFileOnVerifyError(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	payload := []byte("content")
	cfg := model.RepoConfig{ID: "repo", BlobCacheDir: tmp}
	node := model.BaseNode{RepoID: cfg.ID, Path: "file.txt", ObjectOID: "blob", SizeState: "unknown"}
	cachePath := filepath.Join(tmp, node.ObjectOID)
	if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fetcher := &fakeBlobFetcher{payload: payload, verifyErr: errors.New("verify failed")}
	h := New(fetcher)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	size, ok, err := h.validateCachedBlob(ctx, cfg, cachePath, node)
	if err != nil {
		t.Fatalf("validateCachedBlob: %v", err)
	}
	if ok {
		t.Fatalf("validateCachedBlob unexpectedly trusted cache file with verify error")
	}
	if size != 0 {
		t.Fatalf("size = %d, want 0 when validation falls back to refetch", size)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file should remain on verify error: %v", err)
	}

	fetcher.verifyErr = nil
	fetcher.verifyOK = true
	size, ok, err = h.validateCachedBlob(ctx, cfg, cachePath, node)
	if err != nil {
		t.Fatalf("validateCachedBlob after recovery: %v", err)
	}
	if !ok {
		t.Fatal("validateCachedBlob did not trust recovered cache file")
	}
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
}

type fakeBlobFetcher struct {
	mu            sync.Mutex
	calls         int
	verifyCalls   int
	payload       []byte
	verifyOK      bool
	verifyErr     error
	verifyStarted chan struct{}
	verifyWait    <-chan struct{}
	verifyOnce    sync.Once
}

func (f *fakeBlobFetcher) BlobToCache(_ context.Context, _ model.RepoConfig, _ string, dstPath string) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(dstPath, f.payload, 0o644); err != nil {
		return 0, err
	}
	return int64(len(f.payload)), nil
}

func (f *fakeBlobFetcher) VerifyBlob(_ context.Context, _ model.RepoConfig, _ string, _ string) (bool, error) {
	f.mu.Lock()
	f.verifyCalls++
	f.mu.Unlock()
	if f.verifyStarted != nil {
		f.verifyOnce.Do(func() { close(f.verifyStarted) })
	}
	if f.verifyWait != nil {
		<-f.verifyWait
	}
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	return f.verifyOK, nil
}

func (f *fakeBlobFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBlobFetcher) VerifyCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifyCalls
}
