//go:build !windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

type repoSpec struct {
	name   string
	url    string
	branch string
}

var benchRepos = []repoSpec{
	{"agents", "https://github.com/cloudflare/agents.git", "main"},
	{"workers-sdk", "https://github.com/cloudflare/workers-sdk.git", "main"},
	{"ask-bonk", "https://github.com/ask-bonk/ask-bonk.git", "main"},
}

// timing records a labeled duration.
type timing struct {
	label string
	dur   time.Duration
	extra string // optional detail (e.g., node count)
}

func TestBenchRepos(t *testing.T) {
	if os.Getenv("AFS_RUN_BENCH") != "1" {
		t.Skip("skipping benchmarks (set AFS_RUN_BENCH=1 to run)")
	}

	logger := logging.NewJSONLogger(os.Stderr, slog.LevelWarn)
	git := gitstore.New(logger)

	for _, repo := range benchRepos {
		t.Run(repo.name, func(t *testing.T) {
			results := benchmarkRepo(t, git, repo)
			printReport(t, repo, results)
		})
	}
}

func benchmarkRepo(t *testing.T, git *gitstore.Store, repo repoSpec) []timing {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()

	cfg := model.RepoConfig{
		ID:            model.RepoID(repo.name),
		Name:          repo.name,
		RemoteURL:     repo.url,
		Branch:        repo.branch,
		GitDir:        filepath.Join(root, "git"),
		MetaDBPath:    filepath.Join(root, "meta.sqlite"),
		OverlayDir:    filepath.Join(root, "overlay"),
		OverlayDBPath: filepath.Join(root, "overlay", "meta.sqlite"),
		BlobCacheDir:  filepath.Join(root, "cache"),
		MountPath:     filepath.Join(root, "mnt"),
		MountRoot:     filepath.Join(root, "mnt"),
	}

	var results []timing

	// -------------------------------------------------------
	// Phase 1: Blobless clone
	// -------------------------------------------------------
	start := time.Now()
	if err := git.CloneBlobless(ctx, cfg); err != nil {
		t.Skipf("clone failed (repo may be private or unavailable): %v", err)
	}
	cloneDur := time.Since(start)
	results = append(results, timing{"clone (blobless)", cloneDur, ""})

	// -------------------------------------------------------
	// Phase 2: Resolve HEAD
	// -------------------------------------------------------
	start = time.Now()
	headOID, headRef, err := git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	results = append(results, timing{"resolve HEAD", time.Since(start), headRef + " " + headOID[:8]})

	// -------------------------------------------------------
	// Phase 3: Build tree index (ls-tree + batch-check sizes)
	// -------------------------------------------------------
	start = time.Now()
	nodes, err := git.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	indexDur := time.Since(start)

	// Count files, dirs, known sizes
	var fileCount, dirCount, symlinkCount int
	var knownSizes, unknownSizes int
	var totalKnownBytes int64
	for _, n := range nodes {
		switch n.Type {
		case "file":
			fileCount++
		case "dir":
			dirCount++
		case "symlink":
			symlinkCount++
		}
		if n.SizeState == "known" {
			knownSizes++
			totalKnownBytes += n.SizeBytes
		} else {
			unknownSizes++
		}
	}
	results = append(results, timing{"build tree index", indexDur,
		fmt.Sprintf("%d nodes (%d files, %d dirs, %d symlinks)", len(nodes), fileCount, dirCount, symlinkCount)})
	results = append(results, timing{"  ls-tree + batch-check", indexDur,
		fmt.Sprintf("sizes: %d known (%.1f MB), %d unknown", knownSizes, float64(totalKnownBytes)/(1024*1024), unknownSizes)})

	// -------------------------------------------------------
	// Phase 4: Publish snapshot (SQLite bulk insert)
	// -------------------------------------------------------
	start = time.Now()
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatalf("snapshot.New: %v", err)
	}
	gen, err := snap.PublishGeneration(ctx, headOID, headRef, nodes)
	if err != nil {
		t.Fatalf("PublishGeneration: %v", err)
	}
	publishDur := time.Since(start)
	results = append(results, timing{"publish snapshot", publishDur,
		fmt.Sprintf("gen=%d, %d rows inserted", gen, len(nodes))})

	// Cold-start total (what a user waits for before mount is ready)
	coldStart := cloneDur + indexDur + publishDur
	results = append(results, timing{"COLD START TOTAL", coldStart,
		"clone + index + publish"})

	// -------------------------------------------------------
	// Phase 5: Snapshot lookups (simulating readdir + getattr)
	// -------------------------------------------------------
	// Root readdir
	start = time.Now()
	rootChildren, err := snap.ListChildren(gen, ".")
	if err != nil {
		t.Fatalf("ListChildren root: %v", err)
	}
	rootReaddirDur := time.Since(start)
	results = append(results, timing{"readdir root (snapshot)", rootReaddirDur,
		fmt.Sprintf("%d children", len(rootChildren))})

	// Getattr for all root children (simulates what FUSE does after readdir)
	start = time.Now()
	for _, c := range rootChildren {
		snap.GetNode(gen, c.Path)
	}
	rootGetattrDur := time.Since(start)
	results = append(results, timing{"getattr root children (snapshot)", rootGetattrDur,
		fmt.Sprintf("%d lookups", len(rootChildren))})

	// Find a deeply nested directory for readdir benchmark
	deepDir := findDeepDir(nodes, 3)
	if deepDir != "" {
		start = time.Now()
		deepChildren, _ := snap.ListChildren(gen, deepDir)
		results = append(results, timing{"readdir deep dir (snapshot)", time.Since(start),
			fmt.Sprintf("%s -> %d children", deepDir, len(deepChildren))})
	}

	// -------------------------------------------------------
	// Phase 6: Overlay setup + resolver performance
	// -------------------------------------------------------
	os.MkdirAll(cfg.OverlayDir, 0o755)
	os.MkdirAll(cfg.BlobCacheDir, 0o755)
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatalf("overlay.New: %v", err)
	}
	defer ov.Close()

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)

	// Merged readdir (snapshot + overlay)
	start = time.Now()
	mergedEntries, err := resolver.ReaddirTyped(ctx, ".")
	if err != nil {
		t.Fatalf("ReaddirTyped: %v", err)
	}
	results = append(results, timing{"readdir root (merged)", time.Since(start),
		fmt.Sprintf("%d entries", len(mergedEntries))})

	// -------------------------------------------------------
	// Phase 7: Cold hydration -- read files that need blob fetch
	// -------------------------------------------------------
	h := hydrator.New(git)
	h.Start(4, cfg) // 4 workers
	defer h.Stop()

	// Pick a sample of files to hydrate (small text files first, up to 20)
	sampleFiles := pickHydrationSample(nodes, 20)
	results = append(results, timing{"hydration sample", 0,
		fmt.Sprintf("%d files selected", len(sampleFiles))})

	// Cold hydration (one by one, measuring each)
	var hydrateDurs []time.Duration
	var hydrateTotalBytes int64
	for _, n := range sampleFiles {
		start = time.Now()
		_, size, err := h.EnsureHydrated(ctx, cfg, n)
		dur := time.Since(start)
		if err != nil {
			t.Logf("hydrate %s: %v", n.Path, err)
			continue
		}
		hydrateDurs = append(hydrateDurs, dur)
		hydrateTotalBytes += size
	}
	if len(hydrateDurs) > 0 {
		p50, p95, p99 := percentiles(hydrateDurs)
		results = append(results, timing{"cold hydration (per file)", 0,
			fmt.Sprintf("n=%d, p50=%v, p95=%v, p99=%v, total=%.1f KB",
				len(hydrateDurs), p50.Round(time.Millisecond), p95.Round(time.Millisecond), p99.Round(time.Millisecond),
				float64(hydrateTotalBytes)/1024)})

		var totalHydrate time.Duration
		for _, d := range hydrateDurs {
			totalHydrate += d
		}
		results = append(results, timing{"cold hydration (total wall)", totalHydrate,
			fmt.Sprintf("%d files, %.1f KB", len(hydrateDurs), float64(hydrateTotalBytes)/1024)})
	}

	// -------------------------------------------------------
	// Phase 8: Warm cache reads -- re-read the same files
	// -------------------------------------------------------
	var warmDurs []time.Duration
	for _, n := range sampleFiles {
		start = time.Now()
		_, _, err := h.EnsureHydrated(ctx, cfg, n)
		dur := time.Since(start)
		if err != nil {
			continue
		}
		warmDurs = append(warmDurs, dur)
	}
	if len(warmDurs) > 0 {
		p50, p95, p99 := percentiles(warmDurs)
		results = append(results, timing{"warm cache read (per file)", 0,
			fmt.Sprintf("n=%d, p50=%v, p95=%v, p99=%v",
				len(warmDurs), p50, p95, p99)})
	}

	// -------------------------------------------------------
	// Phase 9: Batch hydration -- hydrate many files concurrently
	// -------------------------------------------------------
	batchFiles := pickHydrationSample(nodes, 100)
	// Remove files we already hydrated
	hydrated := map[string]bool{}
	for _, n := range sampleFiles {
		hydrated[n.ObjectOID] = true
	}
	var freshBatch []model.BaseNode
	for _, n := range batchFiles {
		if !hydrated[n.ObjectOID] {
			freshBatch = append(freshBatch, n)
		}
	}
	if len(freshBatch) > 0 {
		start = time.Now()
		type hydrateResult struct {
			size int64
			err  error
		}
		results_ch := make(chan hydrateResult, len(freshBatch))
		for _, n := range freshBatch {
			go func() {
				_, size, err := h.EnsureHydrated(ctx, cfg, n)
				results_ch <- hydrateResult{size, err}
			}()
		}
		var batchBytes int64
		var batchErrors int
		for i := 0; i < len(freshBatch); i++ {
			r := <-results_ch
			if r.err != nil {
				batchErrors++
			} else {
				batchBytes += r.size
			}
		}
		batchDur := time.Since(start)
		results = append(results, timing{"batch hydration (concurrent)", batchDur,
			fmt.Sprintf("%d files, %.1f KB, %d errors, throughput=%.1f files/s",
				len(freshBatch), float64(batchBytes)/1024, batchErrors,
				float64(len(freshBatch)-batchErrors)/batchDur.Seconds())})
	}

	// -------------------------------------------------------
	// Phase 10: Refresh -- fetch + re-index + re-publish
	// -------------------------------------------------------
	start = time.Now()
	fetchErr := git.Fetch(ctx, cfg)
	fetchDur := time.Since(start)
	if fetchErr != nil {
		results = append(results, timing{"fetch", fetchDur, "error: " + fetchErr.Error()})
	} else {
		results = append(results, timing{"fetch (no-op, already up to date)", fetchDur, ""})
	}

	// Re-index (simulates what watcher triggers)
	start = time.Now()
	nodes2, err := git.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		t.Fatalf("re-index: %v", err)
	}
	reindexDur := time.Since(start)
	results = append(results, timing{"re-index (same HEAD)", reindexDur,
		fmt.Sprintf("%d nodes", len(nodes2))})

	start = time.Now()
	gen2, err := snap.PublishGeneration(ctx, headOID, headRef, nodes2)
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	republishDur := time.Since(start)
	results = append(results, timing{"re-publish snapshot", republishDur,
		fmt.Sprintf("gen=%d", gen2)})

	refreshTotal := fetchDur + reindexDur + republishDur
	results = append(results, timing{"REFRESH TOTAL", refreshTotal,
		"fetch + reindex + publish"})

	snap.Close()
	return results
}

// printReport outputs a formatted timing report for a repo.
func printReport(t *testing.T, repo repoSpec, results []timing) {
	t.Helper()
	t.Logf("\n")
	t.Logf("═══════════════════════════════════════════════════════")
	t.Logf("  BENCHMARK: %s (%s)", repo.name, repo.url)
	t.Logf("═══════════════════════════════════════════════════════")
	for _, r := range results {
		if r.dur > 0 {
			t.Logf("  %-40s %10s  %s", r.label, r.dur.Round(time.Millisecond), r.extra)
		} else {
			t.Logf("  %-40s %10s  %s", r.label, "", r.extra)
		}
	}
	t.Logf("═══════════════════════════════════════════════════════")
}

// findDeepDir returns a directory at the given depth with the most children.
func findDeepDir(nodes []model.BaseNode, targetDepth int) string {
	childCount := map[string]int{}
	for _, n := range nodes {
		if n.Type == "dir" {
			continue
		}
		depth := strings.Count(n.Path, "/")
		if depth >= targetDepth {
			parts := strings.SplitN(n.Path, "/", targetDepth+1)
			dir := strings.Join(parts[:targetDepth], "/")
			childCount[dir]++
		}
	}
	var best string
	var bestCount int
	for dir, count := range childCount {
		if count > bestCount {
			best = dir
			bestCount = count
		}
	}
	return best
}

// pickHydrationSample selects files for hydration testing, preferring small
// text files to avoid slow binary downloads dominating the benchmark.
func pickHydrationSample(nodes []model.BaseNode, count int) []model.BaseNode {
	// Collect files with known OIDs, sorted by priority (text files first, then by size)
	type candidate struct {
		node     model.BaseNode
		priority int
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Type != "file" || n.ObjectOID == "" || seen[n.ObjectOID] {
			continue
		}
		seen[n.ObjectOID] = true
		pri := hydrator.ClassifyPriority(n.Path)
		candidates = append(candidates, candidate{n, pri})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].node.SizeBytes < candidates[j].node.SizeBytes
	})
	if len(candidates) > count {
		candidates = candidates[:count]
	}
	out := make([]model.BaseNode, len(candidates))
	for i, c := range candidates {
		out[i] = c.node
	}
	return out
}

func percentiles(durs []time.Duration) (p50, p95, p99 time.Duration) {
	if len(durs) == 0 {
		return 0, 0, 0
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	slices.Sort(sorted)
	p50 = sorted[pctIndex(len(sorted), 50)]
	p95 = sorted[pctIndex(len(sorted), 95)]
	p99 = sorted[pctIndex(len(sorted), 99)]
	return
}

func pctIndex(n, pct int) int {
	idx := int(math.Ceil(float64(n)*float64(pct)/100)) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}
