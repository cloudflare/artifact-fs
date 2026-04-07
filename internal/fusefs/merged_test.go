package fusefs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type fakeSnapshot struct {
	nodes map[string]model.BaseNode
	kids  map[string][]model.BaseNode
}

func (f *fakeSnapshot) PublishGeneration(_ context.Context, _ string, _ string, _ []model.BaseNode) (int64, error) {
	return 0, nil
}

func (f *fakeSnapshot) GetNode(_ int64, path string) (model.BaseNode, bool) {
	n, ok := f.nodes[path]
	return n, ok
}

func (f *fakeSnapshot) ListChildren(_ int64, path string) ([]model.BaseNode, error) {
	if v, ok := f.kids[path]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}

// fakeOverlay satisfies model.OverlayStore for testing.
type fakeOverlay struct {
	entries map[string]model.OverlayEntry
	list    []model.OverlayEntry
}

func (f *fakeOverlay) Get(path string) (model.OverlayEntry, bool) {
	v, ok := f.entries[path]
	return v, ok
}
func (f *fakeOverlay) ListByPrefix(_ context.Context, _ string) ([]model.OverlayEntry, error) {
	return f.list, nil
}
func (f *fakeOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, _ string, _ model.BaseNode) (model.OverlayEntry, error) {
	return model.OverlayEntry{}, nil
}
func (f *fakeOverlay) CreateFile(_ context.Context, _ string, _ uint32) (model.OverlayEntry, error) {
	return model.OverlayEntry{}, nil
}
func (f *fakeOverlay) WriteFile(_ context.Context, _ string, _ int64, _ []byte) (int, error) {
	return 0, nil
}
func (f *fakeOverlay) Remove(_ context.Context, _ string) error                { return nil }
func (f *fakeOverlay) Rename(_ context.Context, _, _ string) error             { return nil }
func (f *fakeOverlay) Mkdir(_ context.Context, _ string, _ uint32) error       { return nil }
func (f *fakeOverlay) SetMtime(_ context.Context, _ string, _ time.Time) error { return nil }
func (f *fakeOverlay) Reconcile(_ context.Context, _ func(string) (model.BaseNode, bool)) error {
	return nil
}
func (f *fakeOverlay) DirtyCount(_ context.Context) (int64, error) { return 0, nil }

func newResolver(snap *fakeSnapshot, ov *fakeOverlay) *Resolver {
	r := &Resolver{Snapshot: snap, Overlay: ov}
	r.SetGeneration(1)
	return r
}

func TestResolvePrefersWhiteout(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"a.txt": {Path: "a.txt", Type: "file", Mode: 0o644}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{"a.txt": {Path: "a.txt", Kind: model.OverlayKindDelete}}},
	)
	_, err := r.ResolvePath("a.txt")
	if err == nil {
		t.Fatal("expected not found due to whiteout")
	}
}

func TestResolveOverlayTakesPrecedence(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"f.txt": {Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "base"}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{"f.txt": {Path: "f.txt", Kind: model.OverlayKindModify, Mode: 0o644}}},
	)
	n, err := r.ResolvePath("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !n.FromOverlay {
		t.Fatal("expected overlay to take precedence")
	}
}

func TestGetattrReturnsMtime(t *testing.T) {
	mtime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	r := newResolver(
		&fakeSnapshot{},
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{"x.txt": {Path: "x.txt", Kind: model.OverlayKindCreate, Mode: 0o644, SizeBytes: 10, MtimeUnixNs: mtime}},
		},
	)
	_, _, _, mt, err := r.Getattr("x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if mt.UnixNano() != mtime {
		t.Fatalf("mtime = %v, want %v", mt, time.Unix(0, mtime))
	}
}

func TestGetattrBaseFileUsesCommitTime(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	// Set a realistic commit timestamp.
	commitTS := int64(1700000000) // 2023-11-14
	r.SetCommitTime(commitTS)

	_, _, _, mt, err := r.Getattr("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Unix(commitTS, 0)
	if !mt.Equal(expected) {
		t.Fatalf("mtime = %v, want %v", mt, expected)
	}
}

func TestGetattrBaseFileFallsBackToGeneration(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{nodes: map[string]model.BaseNode{"b.txt": {Path: "b.txt", Type: "file", Mode: 0o644, SizeState: "known", SizeBytes: 5}}},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	// Don't set commit time -- should fall back to generation.
	_, _, _, mt, err := r.Getattr("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	expected := time.Unix(1, 0) // generation = 1
	if !mt.Equal(expected) {
		t.Fatalf("mtime = %v, want %v", mt, expected)
	}
}

func TestReaddirMergesSnapshotAndOverlay(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {
					{Path: "a.txt", Type: "file"},
					{Path: "b.txt", Type: "file"},
				},
			},
		},
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{},
			list:    []model.OverlayEntry{{Path: "c.txt", Kind: model.OverlayKindCreate}},
		},
	)
	names, err := r.Readdir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	if !set["a.txt"] || !set["b.txt"] || !set["c.txt"] {
		t.Fatalf("expected a.txt, b.txt, c.txt, got %v", names)
	}
}

func TestReaddirWhiteoutRemovesEntry(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {{Path: "keep.txt", Type: "file"}, {Path: "del.txt", Type: "file"}},
			},
		},
		&fakeOverlay{
			entries: map[string]model.OverlayEntry{"del.txt": {Path: "del.txt", Kind: model.OverlayKindDelete}},
			list:    []model.OverlayEntry{{Path: "del.txt", Kind: model.OverlayKindDelete}},
		},
	)
	names, err := r.Readdir(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == "del.txt" {
			t.Fatal("del.txt should be removed by whiteout")
		}
	}
	found := false
	for _, n := range names {
		if n == "keep.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("keep.txt should remain")
	}
}

func TestReaddirTypedReturnsTypes(t *testing.T) {
	r := newResolver(
		&fakeSnapshot{
			kids: map[string][]model.BaseNode{
				".": {
					{Path: "dir", Type: "dir"},
					{Path: "file.txt", Type: "file"},
					{Path: "link", Type: "symlink"},
				},
			},
		},
		&fakeOverlay{entries: map[string]model.OverlayEntry{}},
	)
	entries, err := r.ReaddirTyped(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, e := range entries {
		types[e.Name] = e.Type
	}
	if types["dir"] != "dir" || types["file.txt"] != "file" || types["link"] != "symlink" {
		t.Fatalf("wrong types: %v", types)
	}
}

func TestChildName(t *testing.T) {
	tests := []struct {
		parent, entry string
		wantName      string
		wantOK        bool
	}{
		{".", "foo", "foo", true},
		{".", "foo/bar", "foo", true},
		{"src", "src/main.go", "main.go", true},
		{"src", "srclib/foo.go", "", false}, // prefix collision
		{"src", "src", "", false},           // exact match, not a child
		{"pkg/sub", "pkg/sub/a.txt", "a.txt", true},
		{".", "", "", false},
	}
	for _, tt := range tests {
		name, ok := childName(tt.parent, tt.entry)
		if ok != tt.wantOK || name != tt.wantName {
			t.Errorf("childName(%q, %q) = (%q, %v), want (%q, %v)",
				tt.parent, tt.entry, name, ok, tt.wantName, tt.wantOK)
		}
	}
}
