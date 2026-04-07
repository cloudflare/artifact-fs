package fusefs

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// childName extracts the immediate child name of entryPath under parent.
// Returns ("", false) if entryPath is not a descendant of parent.
func childName(parent, entryPath string) (string, bool) {
	var rel string
	if parent == "." {
		rel = entryPath
	} else {
		if !strings.HasPrefix(entryPath, parent+"/") {
			return "", false
		}
		rel = entryPath[len(parent)+1:]
	}
	if rel == "" {
		return "", false
	}
	if i := strings.IndexByte(rel, '/'); i != -1 {
		rel = rel[:i]
	}
	return rel, true
}

type Resolver struct {
	generation atomic.Int64
	commitTime atomic.Int64 // unix seconds of HEAD commit
	Snapshot   model.SnapshotStore
	Overlay    model.OverlayStore
}

func (r *Resolver) SetGeneration(gen int64) { r.generation.Store(gen) }
func (r *Resolver) Generation() int64       { return r.generation.Load() }
func (r *Resolver) SetCommitTime(ts int64)  { r.commitTime.Store(ts) }
func (r *Resolver) CommitTime() int64       { return r.commitTime.Load() }

type ResolvedNode struct {
	FromOverlay bool
	Base        model.BaseNode
	Overlay     model.OverlayEntry
}

func (r *Resolver) ResolvePath(path string) (ResolvedNode, error) {
	path = model.CleanPath(path)
	if ov, ok := r.Overlay.Get(path); ok {
		if ov.IsDeleted() {
			return ResolvedNode{}, fs.ErrNotExist
		}
		return ResolvedNode{FromOverlay: true, Overlay: ov}, nil
	}
	if n, ok := r.Snapshot.GetNode(r.Generation(), path); ok {
		return ResolvedNode{Base: n}, nil
	}
	return ResolvedNode{}, fs.ErrNotExist
}

func (r *Resolver) Lookup(parent, name string) (ResolvedNode, error) {
	if parent == "" {
		parent = "."
	}
	p := model.CleanPath(filepath.Join(parent, name))
	return r.ResolvePath(p)
}

func (r *Resolver) Getattr(path string) (mode uint32, size int64, nodeType string, mtime time.Time, err error) {
	n, err := r.ResolvePath(path)
	if err != nil {
		return 0, 0, "", time.Time{}, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		mt := time.Unix(0, n.Overlay.MtimeUnixNs)
		return n.Overlay.Mode, n.Overlay.SizeBytes, typ, mt, nil
	}
	mode = normalizeMode(n.Base.Mode, n.Base.Type)
	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := r.CommitTime()
	if ct == 0 {
		ct = r.Generation() // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, n.Base.SizeBytes, n.Base.Type, mt, nil
}

// normalizeMode ensures sane permission bits. Git tree entries have mode 040000
// which has zero permission bits after masking; directories need at least 0o755.
func normalizeMode(mode uint32, typ string) uint32 {
	perms := mode & 0o777
	if typ == "dir" && perms == 0 {
		return 0o755
	}
	if (typ == "file" || typ == "symlink") && perms == 0 {
		return 0o644
	}
	return mode
}

func (r *Resolver) Readdir(ctx context.Context, path string) ([]string, error) {
	entries, err := r.ReaddirTyped(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out, nil
}

// ReaddirTyped returns directory entries with name and type, so the FUSE
// adapter doesn't need to call Getattr per child.
func (r *Resolver) ReaddirTyped(ctx context.Context, path string) ([]ReaddirEntry, error) {
	path = model.CleanPath(path)
	children, err := r.Snapshot.ListChildren(r.Generation(), path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	type entry struct {
		name string
		typ  string
	}
	set := map[string]entry{}
	for _, c := range children {
		name := filepath.Base(c.Path)
		if name != "." {
			set[name] = entry{name: name, typ: c.Type}
		}
	}
	ovEntries, err := r.Overlay.ListByPrefix(ctx, path)
	if err == nil {
		for _, e := range ovEntries {
			name, ok := childName(path, e.Path)
			if !ok {
				continue
			}
			childPath := model.CleanPath(filepath.Join(path, name))
			if e.IsDeleted() && e.Path == childPath {
				delete(set, name)
				continue
			}
			if ov, ok := r.Overlay.Get(childPath); ok && ov.IsDeleted() {
				continue
			}
			typ := e.NodeType()
			set[name] = entry{name: name, typ: typ}
		}
	}
	out := make([]ReaddirEntry, 0, len(set))
	for _, e := range set {
		out = append(out, ReaddirEntry{Name: e.name, Type: e.typ})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
