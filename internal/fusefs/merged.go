package fusefs

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		var ok bool
		rel, ok = strings.CutPrefix(entryPath, parent+"/")
		if !ok {
			return "", false
		}
	}
	if rel == "" {
		return "", false
	}
	rel, _, _ = strings.Cut(rel, "/")
	return rel, true
}

type Resolver struct {
	generation atomic.Int64
	commitTime atomic.Int64 // unix seconds of HEAD commit
	viewMu     sync.RWMutex
	Snapshot   model.SnapshotStore
	Overlay    model.OverlayStore
}

func (r *Resolver) SetGeneration(gen int64) { r.generation.Store(gen) }
func (r *Resolver) Generation() int64       { return r.generation.Load() }
func (r *Resolver) SetCommitTime(ts int64)  { r.commitTime.Store(ts) }
func (r *Resolver) CommitTime() int64       { return r.commitTime.Load() }

// BeginViewUpdate prevents FUSE namespace mutations from resolving an old
// generation while the overlay is reconciled and the new view is published.
func (r *Resolver) BeginViewUpdate() func() {
	r.viewMu.Lock()
	return r.viewMu.Unlock
}

type ResolvedNode struct {
	FromOverlay bool
	Base        model.BaseNode
	Overlay     model.OverlayEntry
}

func (r *Resolver) ResolvePath(path string) (ResolvedNode, error) {
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	return r.resolvePath(path)
}

func (r *Resolver) resolvePath(path string) (ResolvedNode, error) {
	path = model.CleanPath(path)
	if ov, ok := r.Overlay.Get(path); ok {
		if ov.IsDeleted() {
			return ResolvedNode{}, fs.ErrNotExist
		}
		return ResolvedNode{FromOverlay: true, Overlay: ov}, nil
	}
	if n, ok := r.Snapshot.GetNode(r.Generation(), path); ok {
		if n.Type == "dir" {
			return ResolvedNode{Base: n}, nil
		}
		hasDescendant, err := r.Overlay.HasDescendant(context.Background(), path)
		if err != nil {
			return ResolvedNode{}, err
		}
		if !hasDescendant {
			return ResolvedNode{Base: n}, nil
		}
		return ResolvedNode{FromOverlay: true, Overlay: model.OverlayEntry{
			Path: path,
			Kind: model.OverlayKindMkdir,
			Mode: 0o755,
		}}, nil
	}
	hasDescendant, err := r.Overlay.HasDescendant(context.Background(), path)
	if err != nil {
		return ResolvedNode{}, err
	}
	if hasDescendant {
		return ResolvedNode{FromOverlay: true, Overlay: model.OverlayEntry{
			Path: path,
			Kind: model.OverlayKindMkdir,
			Mode: 0o755,
		}}, nil
	}
	return ResolvedNode{}, fs.ErrNotExist
}

func (r *Resolver) Lookup(parent, name string) (ResolvedNode, error) {
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	if parent == "" {
		parent = "."
	}
	p := model.CleanPath(filepath.Join(parent, name))
	return r.resolvePath(p)
}

func (r *Resolver) Getattr(path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error) {
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	n, err := r.resolvePath(path)
	if err != nil {
		return 0, 0, "", time.Time{}, time.Time{}, err
	}
	if n.FromOverlay {
		typ := n.Overlay.NodeType()
		mt := time.Unix(0, n.Overlay.MtimeUnixNs)
		ct := time.Unix(0, n.Overlay.CtimeUnixNs)
		return n.Overlay.Mode, n.Overlay.SizeBytes, typ, mt, ct, nil
	}
	mode = normalizeMode(n.Base.Mode, n.Base.Type)
	// Base files use the HEAD commit timestamp for mtime so tools like
	// make see a stable, meaningful value.
	ct := r.CommitTime()
	if ct == 0 {
		ct = r.Generation() // fallback: commit time unavailable
	}
	mt := time.Unix(ct, 0)
	return mode, n.Base.SizeBytes, n.Base.Type, mt, mt, nil
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
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	return r.readdirTypedAt(ctx, path, r.Generation())
}

func (r *Resolver) ReaddirTypedAt(ctx context.Context, path string, generation int64) ([]ReaddirEntry, error) {
	r.viewMu.RLock()
	defer r.viewMu.RUnlock()
	return r.readdirTypedAt(ctx, path, generation)
}

func (r *Resolver) readdirTypedAt(ctx context.Context, path string, generation int64) ([]ReaddirEntry, error) {
	path = model.CleanPath(path)
	children, err := r.Snapshot.ListChildren(generation, path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	type entry struct {
		name        string
		typ         string
		base        model.BaseNode
		overlay     model.OverlayEntry
		fromOverlay bool
	}
	set := map[string]entry{}
	for _, c := range children {
		name := filepath.Base(c.Path)
		if name != "." {
			set[name] = entry{name: name, typ: c.Type, base: c}
		}
	}
	ovEntries, err := r.Overlay.ListByPrefix(ctx, path)
	if err != nil {
		return nil, err
	}
	direct := make(map[string]model.OverlayEntry)
	for _, e := range ovEntries {
		if e.Path == path {
			continue
		}
		name, ok := childName(path, e.Path)
		if !ok {
			continue
		}
		childPath := model.CleanPath(filepath.Join(path, name))
		if e.Path == childPath {
			direct[name] = e
		}
	}
	for name, e := range direct {
		if e.IsDeleted() {
			delete(set, name)
			continue
		}
		set[name] = entry{name: name, typ: e.NodeType(), overlay: e, fromOverlay: true}
	}
	for _, e := range ovEntries {
		name, ok := childName(path, e.Path)
		if !ok {
			continue
		}
		childPath := model.CleanPath(filepath.Join(path, name))
		if e.Path == childPath || direct[name].Path != "" {
			continue
		}
		if existing, exists := set[name]; (!exists || existing.typ != "dir") && !e.IsDeleted() {
			set[name] = entry{name: name, typ: "dir", base: model.BaseNode{Type: "dir", Mode: 0o755, SizeState: "known"}}
		}
	}
	out := make([]ReaddirEntry, 0, len(set))
	for _, e := range set {
		if e.fromOverlay {
			out = append(out, ReaddirEntry{
				Name:        e.name,
				Type:        e.typ,
				Mode:        e.overlay.Mode,
				SizeBytes:   e.overlay.SizeBytes,
				FromOverlay: true,
				MtimeUnixNs: e.overlay.MtimeUnixNs,
				CtimeUnixNs: e.overlay.CtimeUnixNs,
			})
			continue
		}
		out = append(out, ReaddirEntry{Name: e.name, Type: e.typ, Mode: e.base.Mode, ObjectOID: e.base.ObjectOID, SizeState: e.base.SizeState, SizeBytes: e.base.SizeBytes})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
