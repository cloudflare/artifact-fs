package fusefs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
)

func BenchmarkTruncateZeroBaseFile(b *testing.B) {
	const blobSize = 32 << 20
	root := b.TempDir()
	cfg := model.RepoConfig{
		ID:            "repo",
		OverlayDir:    filepath.Join(root, "overlay"),
		OverlayDBPath: filepath.Join(root, "overlay", "meta.sqlite"),
		BlobCacheDir:  filepath.Join(root, "cache"),
	}
	if err := os.MkdirAll(cfg.BlobCacheDir, 0o755); err != nil {
		b.Fatal(err)
	}
	oid := "benchmark-blob"
	payload := bytes.Repeat([]byte("artifact-fs benchmark payload\n"), blobSize/30+1)
	if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, oid), payload[:blobSize], 0o644); err != nil {
		b.Fatal(err)
	}

	nodes := make(map[string]model.BaseNode, b.N)
	paths := make([]string, b.N)
	for i := range b.N {
		path := fmt.Sprintf("file-%d.bin", i)
		paths[i] = path
		nodes[path] = model.BaseNode{
			Path: path, Type: "file", Mode: 0o644, ObjectOID: oid,
			SizeState: "known", SizeBytes: blobSize,
		}
	}
	store, err := overlay.New(context.Background(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	hydrator := &fakeBatchHydrator{path: filepath.Join(cfg.BlobCacheDir, oid)}
	engine := &Engine{
		Repo: cfg,
		Resolver: &Resolver{
			Snapshot: &fakeSnapshot{nodes: nodes},
			Overlay:  store,
		},
		Overlay:  store,
		Hydrator: hydrator,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for _, path := range paths {
		if err := engine.Truncate(context.Background(), path, 0); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(hydrator.calls)/float64(b.N), "hydrations/op")
}
