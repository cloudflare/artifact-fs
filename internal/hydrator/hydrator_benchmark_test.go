package hydrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func BenchmarkAsyncHydration(b *testing.B) {
	const (
		objects         = 4096
		hydratorWorkers = 8
		callerWorkers   = 32
	)
	payload := []byte("small blob payload\n")
	nodes := make([]model.BaseNode, objects)
	for i := range nodes {
		oid := fmt.Sprintf("%040x", i+1)
		nodes[i] = model.BaseNode{
			RepoID:    "repo",
			Path:      fmt.Sprintf("dir/file-%04d.txt", i),
			ObjectOID: oid,
			SizeState: "known",
			SizeBytes: int64(len(payload)),
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cfg := model.RepoConfig{ID: "repo", BlobCacheDir: b.TempDir()}
		fetcher := &fakeBlobFetcher{payload: payload}
		h := New(fetcher)
		h.Start(hydratorWorkers, cfg)

		jobs := make(chan model.BaseNode)
		var wg sync.WaitGroup
		for range callerWorkers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range jobs {
					if _, _, err := h.EnsureHydrated(context.Background(), cfg, n); err != nil {
						b.Errorf("EnsureHydrated: %v", err)
						return
					}
				}
			}()
		}
		for _, n := range nodes {
			jobs <- n
		}
		close(jobs)
		wg.Wait()
		h.Stop()
	}
}

func BenchmarkAsyncHydrationDuplicateReads(b *testing.B) {
	const (
		objects         = 512
		repeats         = 8
		hydratorWorkers = 8
		callerWorkers   = 32
	)
	payload := []byte("small blob payload\n")
	nodes := make([]model.BaseNode, 0, objects*repeats)
	for i := range objects {
		oid := fmt.Sprintf("%040x", i+1)
		for r := range repeats {
			nodes = append(nodes, model.BaseNode{
				RepoID:    "repo",
				Path:      fmt.Sprintf("dir/file-%04d-%02d.txt", i, r),
				ObjectOID: oid,
				SizeState: "known",
				SizeBytes: int64(len(payload)),
			})
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cfg := model.RepoConfig{ID: "repo", BlobCacheDir: b.TempDir()}
		fetcher := &fakeBlobFetcher{payload: payload, fetchDelay: 100 * time.Microsecond}
		h := New(fetcher)
		h.Start(hydratorWorkers, cfg)
		hydrateBenchmarkNodes(b, h, cfg, nodes, callerWorkers)
		h.Stop()
	}
}

func BenchmarkAsyncHydrationGitStore(b *testing.B) {
	const (
		objects         = 2048
		hydratorWorkers = 8
		callerWorkers   = 32
	)

	workDir, gitDir := createBenchmarkGitRepo(b, objects)
	cfg := model.RepoConfig{ID: "repo", GitDir: gitDir}
	git := gitstore.New(nil)
	b.Cleanup(func() {
		git.Close()
		if err := os.RemoveAll(workDir); err != nil {
			b.Errorf("remove benchmark repo: %v", err)
		}
	})
	git.SetBatchPoolSize(hydratorWorkers)

	head, _, err := git.ResolveHEAD(context.Background(), cfg)
	if err != nil {
		b.Fatalf("ResolveHEAD: %v", err)
	}
	nodes, err := git.BuildTreeIndex(context.Background(), cfg, head)
	if err != nil {
		b.Fatalf("BuildTreeIndex: %v", err)
	}
	targets := make([]model.BaseNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Type == "file" && n.ObjectOID != "" {
			targets = append(targets, n)
		}
	}
	if len(targets) != objects {
		b.Fatalf("targets = %d, want %d", len(targets), objects)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cfg.BlobCacheDir = b.TempDir()
		h := New(git)
		h.Start(hydratorWorkers, cfg)
		hydrateBenchmarkNodes(b, h, cfg, targets, callerWorkers)
		h.Stop()
	}
}

func hydrateBenchmarkNodes(b *testing.B, h *Service, cfg model.RepoConfig, nodes []model.BaseNode, callerWorkers int) {
	b.Helper()
	jobs := make(chan model.BaseNode)
	var wg sync.WaitGroup
	for range callerWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				if _, _, err := h.EnsureHydrated(context.Background(), cfg, n); err != nil {
					b.Errorf("EnsureHydrated: %v", err)
					return
				}
			}
		}()
	}
	for _, n := range nodes {
		jobs <- n
	}
	close(jobs)
	wg.Wait()
}

func createBenchmarkGitRepo(b *testing.B, objects int) (workDir string, gitDir string) {
	b.Helper()
	workDir, err := os.MkdirTemp("", "artifact-fs-hydrator-bench-")
	if err != nil {
		b.Fatal(err)
	}
	runBenchmarkGit(b, workDir, "init")
	runBenchmarkGit(b, workDir, "config", "user.name", "Hydrator Bench")
	runBenchmarkGit(b, workDir, "config", "user.email", "hydrator-bench@example.com")
	for i := range objects {
		dir := filepath.Join(workDir, fmt.Sprintf("dir-%02d", i%16))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("file-%04d.txt", i))
		data := []byte(fmt.Sprintf("blob payload %04d\n", i))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	runBenchmarkGit(b, workDir, "add", ".")
	runBenchmarkGit(b, workDir, "commit", "-m", "add benchmark blobs")
	return workDir, filepath.Join(workDir, ".git")
}

func runBenchmarkGit(b *testing.B, dir string, args ...string) {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
