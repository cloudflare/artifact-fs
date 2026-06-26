package fusefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type fakeBatchHydrator struct {
	tasks      []model.HydrationTask
	calls      int
	path       string
	pathsByOID map[string]string
}

type generationSnapshot struct {
	nodes map[int64]map[string]model.BaseNode
}

type blockingCopyOverlay struct {
	*fakeOverlay
	beforeCopy   chan struct{}
	continueCopy chan struct{}
	backingPath  string
}

func (f *fakeBatchHydrator) Enqueue(task model.HydrationTask) {
	f.tasks = append(f.tasks, task)
}

func (f *fakeBatchHydrator) EnqueueBatch(tasks []model.HydrationTask) {
	f.tasks = append(f.tasks, tasks...)
}

func (f *fakeBatchHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	f.calls++
	if f.pathsByOID != nil {
		return f.pathsByOID[node.ObjectOID], 0, nil
	}
	return f.path, 0, nil
}

func (f *fakeBatchHydrator) ReadBlob(context.Context, model.RepoConfig, model.BaseNode, int64) ([]byte, error) {
	return nil, nil
}

func (f *fakeBatchHydrator) QueueDepth(model.RepoID) int { return len(f.tasks) }

func (g *generationSnapshot) PublishGeneration(context.Context, string, string, []model.BaseNode) (int64, error) {
	return 0, nil
}

func (g *generationSnapshot) GetNode(gen int64, path string) (model.BaseNode, bool) {
	n, ok := g.nodes[gen][path]
	return n, ok
}

func (g *generationSnapshot) ListChildren(int64, string) ([]model.BaseNode, error) {
	return nil, nil
}

func (o *blockingCopyOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	close(o.beforeCopy)
	<-o.continueCopy
	now := time.Now().UnixNano()
	e := model.OverlayEntry{Path: model.CleanPath(path), Kind: model.OverlayKindModify, BackingPath: o.backingPath, Mode: base.Mode, MtimeUnixNs: now, CtimeUnixNs: now, SourceOID: base.ObjectOID}
	o.entries[e.Path] = e
	return e, nil
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
