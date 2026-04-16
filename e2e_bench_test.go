//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type e2eBenchRepoSpec struct {
	name   string
	url    string
	branch string
}

type e2eBenchRun struct {
	Repo           string  `json:"repo"`
	Iteration      int     `json:"iteration"`
	Branch         string  `json:"branch"`
	CloneSeconds   float64 `json:"clone_seconds"`
	HydrateSeconds float64 `json:"hydrate_seconds"`
	Objects        int     `json:"objects"`
	Bytes          int64   `json:"bytes"`
	UnknownSizes   int     `json:"unknown_sizes"`
	ObjectsPerSec  float64 `json:"objects_per_sec"`
	BytesPerSec    float64 `json:"bytes_per_sec"`
	CallerWorkers  int     `json:"caller_workers"`
	HydratorWorker int     `json:"hydrator_workers"`
}

type numericSummary struct {
	Median float64
	P90    float64
	P95    float64
	P99    float64
	Max    float64
}

type e2eBenchSummary struct {
	Repo              string
	Runs              int
	Objects           int
	UnknownSizes      int
	CallerWorkers     int
	HydratorWorkers   int
	CloneSeconds      numericSummary
	HydrateSeconds    numericSummary
	ObjectsPerSecond  numericSummary
	BytesPerSecondMBs numericSummary
}

func TestE2EBenchmarkRepos(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_BENCH") != "1" {
		t.Skip("skipping e2e benchmark test (set AFS_RUN_E2E_BENCH=1 to run)")
	}
	skipIfNoFUSE(t)

	runs := getenvInt("AFS_E2E_BENCH_RUNS", 1)
	if runs <= 0 {
		t.Fatalf("AFS_E2E_BENCH_RUNS must be > 0, got %d", runs)
	}

	emptyURL := createLocalEmptyCommitRepo(t)
	repos := []e2eBenchRepoSpec{
		{name: "empty", url: emptyURL, branch: "main"},
		{name: "workers-sdk", url: getenvDefault("AFS_E2E_BENCH_WORKERS_SDK_URL", "https://github.com/cloudflare/workers-sdk.git"), branch: "main"},
		{name: "workerd", url: getenvDefault("AFS_E2E_BENCH_WORKERD_URL", "https://github.com/cloudflare/workerd.git"), branch: "main"},
		{name: "next.js", url: getenvDefault("AFS_E2E_BENCH_NEXTJS_URL", "https://github.com/vercel/next.js.git"), branch: "canary"},
	}

	var runsOut []e2eBenchRun
	hydratorWorkers := getenvInt("AFS_E2E_BENCH_HYDRATOR_WORKERS", daemon.DefaultHydrationConcurrency)
	if hydratorWorkers <= 0 {
		t.Fatalf("AFS_E2E_BENCH_HYDRATOR_WORKERS must be > 0, got %d", hydratorWorkers)
	}
	callerWorkers := getenvInt("AFS_E2E_BENCH_CALLER_WORKERS", maxInt(8, hydratorWorkers*2))
	if callerWorkers <= 0 {
		t.Fatalf("AFS_E2E_BENCH_CALLER_WORKERS must be > 0, got %d", callerWorkers)
	}
	verboseRuns := os.Getenv("AFS_E2E_BENCH_VERBOSE") == "1"
	for i := 1; i <= runs; i++ {
		for _, repo := range repos {
			t.Run(fmt.Sprintf("%s/run-%02d", repo.name, i), func(t *testing.T) {
				result := runE2EBenchmarkOnce(t, repo, i, hydratorWorkers, callerWorkers)
				runsOut = append(runsOut, result)

				if verboseRuns {
					encoded, err := json.Marshal(result)
					if err != nil {
						t.Fatalf("marshal result: %v", err)
					}
					fmt.Printf("BENCH_RUN %s\n", encoded)
				}
			})
		}
	}

	summaries := summarizeBenchRuns(runsOut)
	for _, summary := range summaries {
		fmt.Printf(
			"BENCH_SUMMARY repo=%s runs=%d objects=%d unknown_sizes=%d clone_s median=%.3f p90=%.3f p95=%.3f p99=%.3f max=%.3f hydrate_s median=%.3f p90=%.3f p95=%.3f p99=%.3f max=%.3f obj_s median=%.1f p90=%.1f p95=%.1f p99=%.1f max=%.1f mb_s median=%.2f p90=%.2f p95=%.2f p99=%.2f max=%.2f",
			summary.Repo,
			summary.Runs,
			summary.Objects,
			summary.UnknownSizes,
			summary.CloneSeconds.Median,
			summary.CloneSeconds.P90,
			summary.CloneSeconds.P95,
			summary.CloneSeconds.P99,
			summary.CloneSeconds.Max,
			summary.HydrateSeconds.Median,
			summary.HydrateSeconds.P90,
			summary.HydrateSeconds.P95,
			summary.HydrateSeconds.P99,
			summary.HydrateSeconds.Max,
			summary.ObjectsPerSecond.Median,
			summary.ObjectsPerSecond.P90,
			summary.ObjectsPerSecond.P95,
			summary.ObjectsPerSecond.P99,
			summary.ObjectsPerSecond.Max,
			summary.BytesPerSecondMBs.Median,
			summary.BytesPerSecondMBs.P90,
			summary.BytesPerSecondMBs.P95,
			summary.BytesPerSecondMBs.P99,
			summary.BytesPerSecondMBs.Max,
		)
		fmt.Println()
	}
}

func runE2EBenchmarkOnce(t *testing.T, repo e2eBenchRepoSpec, iteration int, hydratorWorkers int, callerWorkers int) e2eBenchRun {
	t.Helper()

	root := t.TempDir()
	mountDir := t.TempDir()
	repoName := fmt.Sprintf("bench-%s-%02d", repo.name, iteration)
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := logging.NewJSONLogger(os.Stderr, slog.LevelWarn)
	git := gitstore.New(logger)
	defer git.Close()
	git.SetBatchPoolSize(hydratorWorkers)

	cfg := model.RepoConfig{
		Name:              repoName,
		ID:                model.RepoID(repoName),
		RemoteURL:         repo.url,
		RemoteURLRedacted: auth.RedactRemoteURL(repo.url),
		Branch:            repo.branch,
		RefreshInterval:   5 * time.Minute,
		MountRoot:         mountDir,
		Enabled:           true,
		GitDir:            filepath.Join(root, "repos", repoName, "git"),
		MetaDBPath:        filepath.Join(root, "repos", repoName, "meta.sqlite"),
		OverlayDir:        filepath.Join(root, "repos", repoName, "overlay"),
		OverlayDBPath:     filepath.Join(root, "repos", repoName, "overlay", "meta.sqlite"),
		BlobCacheDir:      filepath.Join(root, "repos", repoName, "cache"),
		MountPath:         mountPath,
	}

	cloneStart := time.Now()
	if err := git.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	cloneDur := time.Since(cloneStart)

	headOID, _, err := git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	nodes, err := git.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}

	targets := uniqueHydrationTargets(nodes)
	unknownSizes := countUnknownSizes(targets)
	svc, err := daemon.New(ctx, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.SetMountRoot(mountDir)
	svc.SetHydrationConcurrency(hydratorWorkers)

	if err := svc.AddRepo(ctx, cfg); err != nil {
		t.Fatalf("add-repo: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- svc.Start(ctx) }()

	if !waitForMount(t, mountPath, 60*time.Second) {
		cancel()
		t.Fatal("FUSE mount did not appear within timeout")
	}

	hydrateDur, hydratedBytes, err := hydrateColdObjects(ctx, git, cfg, targets, hydratorWorkers, callerWorkers)
	if err != nil {
		cancel()
		t.Fatalf("hydrateColdObjects: %v", err)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Log("daemon did not exit within 10s")
	}

	result := e2eBenchRun{
		Repo:           repo.name,
		Iteration:      iteration,
		Branch:         repo.branch,
		CloneSeconds:   cloneDur.Seconds(),
		HydrateSeconds: hydrateDur.Seconds(),
		Objects:        len(targets),
		Bytes:          hydratedBytes,
		UnknownSizes:   unknownSizes,
		CallerWorkers:  callerWorkers,
		HydratorWorker: hydratorWorkers,
	}
	if hydrateDur > 0 {
		result.ObjectsPerSec = float64(result.Objects) / hydrateDur.Seconds()
		result.BytesPerSec = float64(hydratedBytes) / hydrateDur.Seconds()
	}
	return result
}

func createLocalEmptyCommitRepo(t *testing.T) string {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "empty-repo.git")
	workDir := filepath.Join(t.TempDir(), "work")

	run(t, "", "git", "init", "--bare", bareDir)
	run(t, "", "git", "clone", bareDir, workDir)
	run(t, workDir, "git", "config", "user.name", "E2E Bench")
	run(t, workDir, "git", "config", "user.email", "e2e-bench@test")
	run(t, workDir, "git", "checkout", "-b", "main")
	run(t, workDir, "git", "commit", "--allow-empty", "-m", "initial empty commit")
	run(t, workDir, "git", "push", "origin", "main")

	return "file://" + bareDir
}

func uniqueHydrationTargets(nodes []model.BaseNode) []model.BaseNode {
	byOID := make(map[string]model.BaseNode)
	for _, n := range nodes {
		if n.Type != "file" || n.ObjectOID == "" {
			continue
		}
		if _, ok := byOID[n.ObjectOID]; ok {
			continue
		}
		byOID[n.ObjectOID] = n
	}

	out := make([]model.BaseNode, 0, len(byOID))
	for _, n := range byOID {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func countUnknownSizes(nodes []model.BaseNode) int {
	count := 0
	for _, n := range nodes {
		if n.SizeState != "known" {
			count++
		}
	}
	return count
}

func hydrateColdObjects(ctx context.Context, git *gitstore.Store, cfg model.RepoConfig, targets []model.BaseNode, hydratorWorkers int, callerWorkers int) (time.Duration, int64, error) {
	if len(targets) == 0 {
		return 0, 0, nil
	}

	h := hydrator.New(git)
	h.Start(hydratorWorkers, cfg)
	defer h.Stop()

	jobs := make(chan model.BaseNode)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var totalBytes int64

	worker := func() {
		defer wg.Done()
		for n := range jobs {
			select {
			case <-ctx.Done():
				select {
				case errCh <- ctx.Err():
				default:
				}
				return
			default:
			}

			_, size, err := h.EnsureHydrated(ctx, cfg, n)
			if err != nil {
				select {
				case errCh <- fmt.Errorf("hydrate %s: %w", n.Path, err):
				default:
				}
				return
			}

			mu.Lock()
			totalBytes += size
			mu.Unlock()
		}
	}

	for range callerWorkers {
		wg.Add(1)
		go worker()
	}

	start := time.Now()
	for _, n := range targets {
		select {
		case err := <-errCh:
			close(jobs)
			wg.Wait()
			return 0, 0, err
		case jobs <- n:
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-errCh:
		return 0, 0, err
	default:
	}
	return time.Since(start), totalBytes, nil
}

func summarizeBenchRuns(runs []e2eBenchRun) []e2eBenchSummary {
	byRepo := make(map[string][]e2eBenchRun)
	for _, run := range runs {
		byRepo[run.Repo] = append(byRepo[run.Repo], run)
	}

	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	out := make([]e2eBenchSummary, 0, len(repos))
	for _, repo := range repos {
		runs := byRepo[repo]
		cloneVals := make([]float64, 0, len(runs))
		hydrateVals := make([]float64, 0, len(runs))
		objVals := make([]float64, 0, len(runs))
		mbVals := make([]float64, 0, len(runs))
		for _, run := range runs {
			cloneVals = append(cloneVals, run.CloneSeconds)
			hydrateVals = append(hydrateVals, run.HydrateSeconds)
			objVals = append(objVals, run.ObjectsPerSec)
			mbVals = append(mbVals, run.BytesPerSec/(1024*1024))
		}
		out = append(out, e2eBenchSummary{
			Repo:              repo,
			Runs:              len(runs),
			Objects:           runs[0].Objects,
			UnknownSizes:      runs[0].UnknownSizes,
			CallerWorkers:     runs[0].CallerWorkers,
			HydratorWorkers:   runs[0].HydratorWorker,
			CloneSeconds:      summarizeValues(cloneVals),
			HydrateSeconds:    summarizeValues(hydrateVals),
			ObjectsPerSecond:  summarizeValues(objVals),
			BytesPerSecondMBs: summarizeValues(mbVals),
		})
	}
	return out
}

func summarizeValues(vals []float64) numericSummary {
	if len(vals) == 0 {
		return numericSummary{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	return numericSummary{
		Median: sorted[pctIndex(len(sorted), 50)],
		P90:    sorted[pctIndex(len(sorted), 90)],
		P95:    sorted[pctIndex(len(sorted), 95)],
		P99:    sorted[pctIndex(len(sorted), 99)],
		Max:    sorted[len(sorted)-1],
	}
}

func getenvInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func getenvDefault(name, fallback string) string {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
