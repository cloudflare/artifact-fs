package fusefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	overlaystore "github.com/cloudflare/artifact-fs/internal/overlay"
)

type fakeBatchHydrator struct {
	tasks       []model.HydrationTask
	calls       int
	path        string
	pathsByOID  map[string]string
	errorsByOID map[string]error
}

type generationSnapshot struct {
	nodes map[int64]map[string]model.BaseNode
	kids  map[int64]map[string][]model.BaseNode
}

type blockingCopyOverlay struct {
	*fakeOverlay
	beforeCopy   chan struct{}
	continueCopy chan struct{}
	backingPath  string
}

type blockingRenameHydrator struct {
	*fakeBatchHydrator
	started chan struct{}
	proceed chan struct{}
}

func (f *fakeBatchHydrator) Enqueue(task model.HydrationTask) {
	f.tasks = append(f.tasks, task)
}

func (f *fakeBatchHydrator) EnqueueBatch(tasks []model.HydrationTask) {
	f.tasks = append(f.tasks, tasks...)
}

func (f *fakeBatchHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	f.calls++
	if err := f.errorsByOID[node.ObjectOID]; err != nil {
		return "", 0, err
	}
	if f.pathsByOID != nil {
		return f.pathsByOID[node.ObjectOID], 0, nil
	}
	return f.path, 0, nil
}

func (f *fakeBatchHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	path, size, err := f.EnsureHydrated(ctx, repo, node)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	return file, size, err
}

func (f *fakeBatchHydrator) ReadBlob(context.Context, model.RepoConfig, model.BaseNode, int64) ([]byte, error) {
	return nil, nil
}

func (f *fakeBatchHydrator) QueueDepth(model.RepoID) int { return len(f.tasks) }

func (h *blockingRenameHydrator) OpenHydrated(ctx context.Context, repo model.RepoConfig, node model.BaseNode) (*os.File, int64, error) {
	close(h.started)
	select {
	case <-h.proceed:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return h.fakeBatchHydrator.OpenHydrated(ctx, repo, node)
}

func (g *generationSnapshot) PublishGeneration(context.Context, string, string, []model.BaseNode) (int64, error) {
	return 0, nil
}

func (g *generationSnapshot) GetNode(gen int64, path string) (model.BaseNode, bool) {
	n, ok := g.nodes[gen][path]
	return n, ok
}

func (g *generationSnapshot) LookupNode(_ context.Context, gen int64, path string) (model.BaseNode, bool, error) {
	n, ok := g.GetNode(gen, path)
	return n, ok, nil
}

func (g *generationSnapshot) ListChildren(gen int64, path string) ([]model.BaseNode, error) {
	return g.kids[gen][path], nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	close(o.beforeCopy)
	<-o.continueCopy
	now := time.Now().UnixNano()
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindModify, BackingPath: o.backingPath, Mode: base.Mode, MtimeUnixNs: now, CtimeUnixNs: now, SourceOID: base.ObjectOID}
	o.entries[e.Path] = e
	return e, nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWriteFrom(ctx context.Context, repo model.RepoConfig, path string, base model.BaseNode, _ *os.File) (model.OverlayEntry, error) {
	return o.EnsureCopyOnWrite(ctx, repo, path, base)
}

func (o *blockingCopyOverlay) WriteFile(_ context.Context, path string, off int64, data []byte) (int, error) {
	f, err := os.OpenFile(o.backingPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		return 0, err
	}
	e := o.entries[model.CleanPath(path)]
	if info, err := f.Stat(); err == nil {
		e.SizeBytes = info.Size()
	}
	o.entries[model.CleanPath(path)] = e
	return len(data), nil
}

func TestPrefetchDirBatchesReaddirMetadata(t *testing.T) {
	hydrator := &fakeBatchHydrator{}
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Hydrator: hydrator}

	engine.PrefetchDir("src", []ReaddirEntry{
		{Name: "a.go", Type: "file", ObjectOID: "a", SizeState: "known", SizeBytes: 10},
		{Name: "sub", Type: "dir"},
		{Name: "overlay.txt", Type: "file"},
		{Name: "b.go", Type: "file", ObjectOID: "b", SizeState: "unknown"},
	})

	if len(hydrator.tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(hydrator.tasks))
	}
	if hydrator.tasks[0].Path != "src/a.go" || hydrator.tasks[0].ObjectOID != "a" || hydrator.tasks[0].SizeBytes != 10 {
		t.Fatalf("first task = %+v", hydrator.tasks[0])
	}
	if hydrator.tasks[1].Path != "src/b.go" || hydrator.tasks[1].ObjectOID != "b" || hydrator.tasks[1].SizeState != "unknown" {
		t.Fatalf("second task = %+v", hydrator.tasks[1])
	}
}

func TestPrefetchDirCapsAndPrioritizesTasks(t *testing.T) {
	h := &fakeBatchHydrator{}
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Hydrator: h}
	entries := make([]ReaddirEntry, 0, maxPrefetchTasksPerDir+2)
	for i := range maxPrefetchTasksPerDir + 1 {
		entries = append(entries, ReaddirEntry{Name: fmt.Sprintf("image-%03d.png", i), Type: "file", ObjectOID: fmt.Sprintf("png-%03d", i)})
	}
	entries = append(entries, ReaddirEntry{Name: "README.md", Type: "file", ObjectOID: "readme"})

	engine.PrefetchDir(".", entries)

	if len(h.tasks) != maxPrefetchTasksPerDir {
		t.Fatalf("tasks = %d, want %d", len(h.tasks), maxPrefetchTasksPerDir)
	}
	foundReadme := false
	for _, task := range h.tasks {
		if task.ObjectOID == "readme" {
			foundReadme = true
			if task.Priority < hydrator.PriorityBootstrap {
				t.Fatalf("README priority = %d", task.Priority)
			}
		}
	}
	if !foundReadme {
		t.Fatal("README.md was dropped from capped prefetch")
	}
}

func TestFileHandleCachesHydratedBaseFile(t *testing.T) {
	tmp := t.TempDir()
	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{path: cachePath}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	engine := &Engine{
		Repo:     model.RepoConfig{ID: "repo"},
		Resolver: newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob"}}}, overlay),
		Overlay:  overlay,
		Hydrator: h,
	}
	fh := &FileHandle{path: "file.txt"}
	defer fh.closeCachedFile()

	first, err := fh.read(context.Background(), engine, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fh.read(context.Background(), engine, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "cont" || string(second) != "ent" {
		t.Fatalf("reads = %q/%q", first, second)
	}
	if h.calls != 1 {
		t.Fatalf("EnsureHydrated calls = %d, want 1", h.calls)
	}
}

func TestFileHandleRehydratesAfterGenerationChange(t *testing.T) {
	tmp := t.TempDir()
	firstCache := filepath.Join(tmp, "first")
	secondCache := filepath.Join(tmp, "second")
	if err := os.WriteFile(firstCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondCache, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{pathsByOID: map[string]string{"first": firstCache, "second": secondCache}}
	overlay := &fakeOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := &Resolver{
		Snapshot: &generationSnapshot{nodes: map[int64]map[string]model.BaseNode{
			1: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "first"}},
			2: {"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "second"}},
		}},
		Overlay: overlay,
	}
	resolver.SetGeneration(1)
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
	fh := &FileHandle{path: "file.txt"}
	defer fh.closeCachedFile()

	first, err := fh.read(context.Background(), engine, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	resolver.SetGeneration(2)
	second, err := fh.read(context.Background(), engine, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "old" || string(second) != "new" {
		t.Fatalf("reads = %q/%q", first, second)
	}
	if h.calls != 2 {
		t.Fatalf("EnsureHydrated calls = %d, want 2", h.calls)
	}
}

func TestFileHandleInvalidatesCacheAfterOverlappingWrite(t *testing.T) {
	tmp := t.TempDir()
	baseCache := filepath.Join(tmp, "base")
	overlayBacking := filepath.Join(tmp, "overlay")
	if err := os.WriteFile(baseCache, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeBatchHydrator{path: baseCache}
	overlay := &blockingCopyOverlay{
		fakeOverlay:  &fakeOverlay{entries: map[string]model.OverlayEntry{}},
		beforeCopy:   make(chan struct{}),
		continueCopy: make(chan struct{}),
		backingPath:  overlayBacking,
	}
	resolver := newResolver(&fakeSnapshot{nodes: map[string]model.BaseNode{"file.txt": {Path: "file.txt", Type: "file", ObjectOID: "blob", Mode: 0o644}}}, overlay.fakeOverlay)
	engine := &Engine{Repo: model.RepoConfig{ID: "repo"}, Resolver: resolver, Overlay: overlay, Hydrator: h}
	fs := NewArtifactFuse(model.RepoConfig{ID: "repo"}, resolver, engine)
	fh := &FileHandle{path: "file.txt"}
	fs.fileHandles[1] = fh
	defer fh.closeCachedFile()

	first, err := fh.read(context.Background(), engine, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "old" {
		t.Fatalf("first read = %q", first)
	}

	writeDone := make(chan error, 1)
	go func() {
		fs.closeCachedFilesForPath("file.txt")
		_, err := engine.Write(context.Background(), "file.txt", 0, []byte("new"))
		if err == nil {
			fs.closeCachedFilesForPath("file.txt")
		}
		writeDone <- err
	}()

	<-overlay.beforeCopy
	overlapped, err := fh.read(context.Background(), engine, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(overlapped) != "old" {
		t.Fatalf("overlapped read = %q", overlapped)
	}
	close(overlay.continueCopy)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	after, err := fh.read(context.Background(), engine, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "new" {
		t.Fatalf("read after write = %q, want new", after)
	}
}

func TestRenameBaseDirectoryHydratesAndMovesContents(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("tracked content"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"},
		},
		kids: map[string][]model.BaseNode{
			"src": {{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"}},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: &fakeBatchHydrator{path: cachePath},
	}

	if err := engine.Rename(context.Background(), "src", "dst"); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Read(context.Background(), "dst/a.txt", 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tracked content" {
		t.Fatalf("moved content = %q", got)
	}
	if _, err := resolver.ResolvePath("src/a.txt"); !os.IsNotExist(err) {
		t.Fatalf("source child resolve err = %v, want not exist", err)
	}
}

func TestRenameBaseDirectoryHydratesBeforeMutatingOverlay(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	firstCache := filepath.Join(tmp, "first")
	secondCache := filepath.Join(tmp, "second")
	if err := os.WriteFile(firstCache, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondCache, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "first"},
			"src/b.txt": {Path: "src/b.txt", Type: "file", Mode: 0o100644, ObjectOID: "second"},
		},
		kids: map[string][]model.BaseNode{
			"src": {
				{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "first"},
				{Path: "src/b.txt", Type: "file", Mode: 0o100644, ObjectOID: "second"},
			},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	hydrationFailure := errors.New("hydrate second blob")
	hydrator := &fakeBatchHydrator{
		pathsByOID:  map[string]string{"first": firstCache, "second": secondCache},
		errorsByOID: map[string]error{"second": hydrationFailure},
	}
	engine := &Engine{Repo: cfg, Resolver: resolver, Overlay: ov, Hydrator: hydrator}

	if err := engine.Rename(context.Background(), "src", "dst"); !errors.Is(err, hydrationFailure) {
		t.Fatalf("Rename error = %v, want hydration failure", err)
	}
	entries, err := ov.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed rename left partial overlay entries: %+v", entries)
	}
	for _, path := range []string{"src", "src/a.txt", "src/b.txt"} {
		if _, err := resolver.ResolvePath(path); err != nil {
			t.Fatalf("source path %q after failed rename: %v", path, err)
		}
	}
	if _, err := resolver.ResolvePath("dst"); !os.IsNotExist(err) {
		t.Fatalf("destination after failed rename error = %v, want not exist", err)
	}

	delete(hydrator.errorsByOID, "second")
	if err := engine.Rename(context.Background(), "src", "dst"); err != nil {
		t.Fatalf("retry Rename: %v", err)
	}
	for path, want := range map[string]string{"dst/a.txt": "first", "dst/b.txt": "second"} {
		got, err := engine.Read(context.Background(), path, 0, 64)
		if err != nil {
			t.Fatalf("read %q after retry: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("read %q after retry = %q, want %q", path, got, want)
		}
	}
}

func TestRenameBaseDirectoryDoesNotResurrectReplacedDestinationDescendants(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":                   {Path: "src", Type: "dir", Mode: 0o40000},
			"src/overlap":           {Path: "src/overlap", Type: "dir", Mode: 0o40000},
			"dst":                   {Path: "dst", Type: "dir", Mode: 0o40000},
			"dst/gone.txt":          {Path: "dst/gone.txt", Type: "file", Mode: 0o100644, ObjectOID: "gone"},
			"dst/overlap":           {Path: "dst/overlap", Type: "dir", Mode: 0o40000},
			"dst/overlap/stale.txt": {Path: "dst/overlap/stale.txt", Type: "file", Mode: 0o100644, ObjectOID: "stale"},
		},
		kids: map[string][]model.BaseNode{
			"src": {
				{Path: "src/overlap", Type: "dir", Mode: 0o40000},
			},
			"dst": {
				{Path: "dst/gone.txt", Type: "file", Mode: 0o100644, ObjectOID: "gone"},
				{Path: "dst/overlap", Type: "dir", Mode: 0o40000},
			},
			"dst/overlap": {
				{Path: "dst/overlap/stale.txt", Type: "file", Mode: 0o100644, ObjectOID: "stale"},
			},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: &fakeBatchHydrator{},
	}
	if err := ov.Remove(context.Background(), "dst"); err != nil {
		t.Fatal(err)
	}

	if err := engine.Rename(context.Background(), "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolvePath("dst/overlap"); err != nil {
		t.Fatalf("resolve incoming overlapping directory: %v", err)
	}
	for _, path := range []string{"dst/gone.txt", "dst/overlap/stale.txt"} {
		if _, err := resolver.ResolvePath(path); !os.IsNotExist(err) {
			t.Fatalf("replaced destination path %q resolve error = %v, want not exist", path, err)
		}
	}
}

func TestRenameDirectoryRejectsDestinationChildCreatedDuringHydration(t *testing.T) {
	tmp := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(tmp, "overlay"),
		OverlayDBPath: filepath.Join(tmp, "overlay.db"),
	}
	ov, err := overlaystore.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ov.Close() })

	cachePath := filepath.Join(tmp, "blob")
	if err := os.WriteFile(cachePath, []byte("tracked content"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := &fakeSnapshot{
		nodes: map[string]model.BaseNode{
			"src":       {Path: "src", Type: "dir", Mode: 0o40000},
			"src/a.txt": {Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"},
		},
		kids: map[string][]model.BaseNode{
			"src": {{Path: "src/a.txt", Type: "file", Mode: 0o100644, ObjectOID: "blob"}},
		},
	}
	resolver := &Resolver{Snapshot: snapshot, Overlay: ov}
	resolver.SetGeneration(1)
	hydrator := &blockingRenameHydrator{
		fakeBatchHydrator: &fakeBatchHydrator{path: cachePath},
		started:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
	engine := &Engine{
		Repo:     cfg,
		Resolver: resolver,
		Overlay:  ov,
		Hydrator: hydrator,
	}
	if err := engine.Mkdir(context.Background(), "dst", 0o755); err != nil {
		t.Fatal(err)
	}

	renameDone := make(chan error, 1)
	go func() {
		renameDone <- engine.Rename(context.Background(), "src", "dst")
	}()
	select {
	case <-hydrator.started:
	case <-time.After(time.Second):
		t.Fatal("rename did not reach hydration")
	}

	if err := engine.Mkdir(context.Background(), "dst/concurrent", 0o755); err != nil {
		t.Fatalf("concurrent destination mkdir: %v", err)
	}
	close(hydrator.proceed)
	select {
	case err := <-renameDone:
		if !os.IsExist(err) {
			t.Fatalf("Rename error = %v, want os.ErrExist", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rename did not finish")
	}

	for _, path := range []string{"src", "src/a.txt", "dst", "dst/concurrent"} {
		if _, err := resolver.ResolvePath(path); err != nil {
			t.Fatalf("resolve preserved path %q: %v", path, err)
		}
	}
	if _, err := resolver.ResolvePath("dst/a.txt"); !os.IsNotExist(err) {
		t.Fatalf("destination source child resolve error = %v, want not exist", err)
	}
}
