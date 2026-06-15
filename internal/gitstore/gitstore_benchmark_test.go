package gitstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func BenchmarkBuildTreeIndexSynthetic(b *testing.B) {
	const objects = 4096
	workDir, gitDir := createBuildTreeBenchmarkRepo(b, objects)
	b.Cleanup(func() { _ = os.RemoveAll(workDir) })

	cfg := model.RepoConfig{ID: "repo", Name: "repo", GitDir: gitDir}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		b.Fatalf("ResolveHEAD: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		nodes, err := store.BuildTreeIndex(context.Background(), cfg, head)
		if err != nil {
			b.Fatalf("BuildTreeIndex: %v", err)
		}
		b.ReportMetric(float64(len(nodes)), "nodes/op")
	}
}

func createBuildTreeBenchmarkRepo(b *testing.B, objects int) (workDir string, gitDir string) {
	b.Helper()
	workDir, err := os.MkdirTemp("", "artifact-fs-buildtree-bench-")
	if err != nil {
		b.Fatal(err)
	}
	runBuildTreeBenchmarkGit(b, workDir, "init")
	runBuildTreeBenchmarkGit(b, workDir, "config", "user.name", "BuildTree Bench")
	runBuildTreeBenchmarkGit(b, workDir, "config", "user.email", "buildtree-bench@example.com")
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
	runBuildTreeBenchmarkGit(b, workDir, "add", ".")
	runBuildTreeBenchmarkGit(b, workDir, "commit", "-m", "add benchmark blobs")
	return workDir, filepath.Join(workDir, ".git")
}

func runBuildTreeBenchmarkGit(b *testing.B, dir string, args ...string) {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
