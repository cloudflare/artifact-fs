package fusefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const maxPrefetchTasksPerDir = 256

type Engine struct {
	Resolver *Resolver
	Repo     model.RepoConfig
	Overlay  model.OverlayStore
	Hydrator model.Hydrator
}

// prepareOverlay hydrates without blocking generation publication, then locks
// and revalidates the view before copy-on-write. The caller must release the
// returned lock after completing its overlay mutation.
func (e *Engine) prepareOverlay(ctx context.Context, path string) (func(), error) {
	path = model.CleanPath(path)
	for {
		e.Resolver.viewMu.RLock()
		if ov, ok := e.Overlay.Get(path); ok {
			if ov.IsDeleted() {
				e.Resolver.viewMu.RUnlock()
				return nil, fs.ErrNotExist
			}
			return e.Resolver.viewMu.RUnlock, nil
		}
		generation := e.Resolver.Generation()
		n, ok := e.Resolver.Snapshot.GetNode(generation, path)
		e.Resolver.viewMu.RUnlock()
		if !ok {
			return nil, fs.ErrNotExist
		}
		if n.ObjectOID != "" {
			if _, _, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, n); err != nil {
				return nil, err
			}
		}

		e.Resolver.viewMu.RLock()
		if e.Resolver.Generation() != generation {
			e.Resolver.viewMu.RUnlock()
			continue
		}
		if ov, ok := e.Overlay.Get(path); ok {
			if ov.IsDeleted() {
				e.Resolver.viewMu.RUnlock()
				return nil, fs.ErrNotExist
			}
			return e.Resolver.viewMu.RUnlock, nil
		}
		current, ok := e.Resolver.Snapshot.GetNode(generation, path)
		if !ok || current.ObjectOID != n.ObjectOID {
			e.Resolver.viewMu.RUnlock()
			continue
		}
		if _, err := e.Overlay.EnsureCopyOnWrite(ctx, e.Repo, path, current); err != nil {
			e.Resolver.viewMu.RUnlock()
			return nil, err
		}
		return e.Resolver.viewMu.RUnlock, nil
	}
}

func (e *Engine) Read(ctx context.Context, path string, off int64, size int) ([]byte, error) {
	e.Resolver.viewMu.RLock()
	if ov, ok := e.Overlay.Get(path); ok {
		if ov.IsDeleted() {
			e.Resolver.viewMu.RUnlock()
			return nil, os.ErrNotExist
		}
		f, err := os.Open(ov.BackingPath)
		e.Resolver.viewMu.RUnlock()
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readFileChunkFrom(f, off, size)
	}
	generation := e.Resolver.Generation()
	n, ok := e.Resolver.Snapshot.GetNode(generation, model.CleanPath(path))
	e.Resolver.viewMu.RUnlock()
	if !ok {
		return nil, fs.ErrNotExist
	}
	cachePath, _, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, n)
	if err != nil {
		return nil, err
	}
	return readFileChunk(cachePath, off, size)
}

func (e *Engine) BaseCachePath(ctx context.Context, path string) (string, int64, bool, error) {
	path = model.CleanPath(path)
	for {
		e.Resolver.viewMu.RLock()
		if ov, ok := e.Overlay.Get(path); ok {
			e.Resolver.viewMu.RUnlock()
			if ov.IsDeleted() {
				return "", 0, false, os.ErrNotExist
			}
			return "", 0, false, nil
		}
		gen := e.Resolver.Generation()
		n, ok := e.Resolver.Snapshot.GetNode(gen, path)
		e.Resolver.viewMu.RUnlock()
		if !ok {
			return "", 0, false, fs.ErrNotExist
		}
		cachePath, _, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, n)
		if err != nil {
			return "", 0, false, err
		}
		e.Resolver.viewMu.RLock()
		valid := e.Resolver.Generation() == gen
		_, overlaid := e.Overlay.Get(path)
		e.Resolver.viewMu.RUnlock()
		if valid && !overlaid {
			return cachePath, gen, true, nil
		}
	}
}

func (e *Engine) Write(ctx context.Context, path string, off int64, data []byte) (int, error) {
	unlock, err := e.prepareOverlay(ctx, path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		e.Resolver.viewMu.RLock()
		defer e.Resolver.viewMu.RUnlock()
		if _, resolveErr := e.Resolver.resolvePath(path); resolveErr == nil {
			return 0, os.ErrExist
		} else if !errors.Is(resolveErr, fs.ErrNotExist) {
			return 0, resolveErr
		}
		if _, cErr := e.Overlay.CreateFile(ctx, path, 0o644); cErr != nil {
			return 0, cErr
		}
	} else {
		defer unlock()
	}
	return e.Overlay.WriteFile(ctx, path, off, data)
}

func (e *Engine) Create(ctx context.Context, path string, mode uint32) error {
	e.Resolver.viewMu.RLock()
	defer e.Resolver.viewMu.RUnlock()
	if _, err := e.Resolver.resolvePath(path); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err := e.Overlay.CreateFile(ctx, path, mode)
	return err
}

func (e *Engine) Unlink(ctx context.Context, path string) error {
	e.Resolver.viewMu.RLock()
	defer e.Resolver.viewMu.RUnlock()
	return e.Overlay.Remove(ctx, path)
}

func (e *Engine) Rename(ctx context.Context, oldPath, newPath string) error {
	e.Resolver.viewMu.RLock()
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	if oldPath == newPath {
		_, err := e.Resolver.resolvePath(oldPath)
		e.Resolver.viewMu.RUnlock()
		return err
	}
	if ov, ok := e.Overlay.Get(oldPath); ok {
		if ov.IsDeleted() {
			e.Resolver.viewMu.RUnlock()
			return os.ErrNotExist
		}
		if ov.Kind == model.OverlayKindMkdir {
			if _, ok := e.Resolver.Snapshot.GetNode(e.Resolver.Generation(), oldPath); ok {
				e.Resolver.viewMu.RUnlock()
				return fs.ErrInvalid
			}
		}
		if dst, ok := e.Resolver.Snapshot.GetNode(e.Resolver.Generation(), newPath); ok {
			if dst.Type == "dir" || ov.Kind == model.OverlayKindMkdir {
				e.Resolver.viewMu.RUnlock()
				return fs.ErrInvalid
			}
			if ov.Kind == model.OverlayKindCreate || ov.Kind == model.OverlayKindSymlink {
				err := e.Overlay.RenameAndMarkModifiedFromBase(ctx, oldPath, newPath, dst.ObjectOID)
				e.Resolver.viewMu.RUnlock()
				return err
			}
		}
		err := e.Overlay.Rename(ctx, oldPath, newPath)
		e.Resolver.viewMu.RUnlock()
		return err
	}
	if n, ok := e.Resolver.Snapshot.GetNode(e.Resolver.Generation(), oldPath); ok && n.Type == "dir" {
		e.Resolver.viewMu.RUnlock()
		return fs.ErrInvalid
	}
	n, err := e.Resolver.resolvePath(oldPath)
	if err != nil {
		e.Resolver.viewMu.RUnlock()
		return err
	}
	if n.Base.Type == "dir" {
		e.Resolver.viewMu.RUnlock()
		return fs.ErrInvalid
	}
	if dst, ok := e.Resolver.Snapshot.GetNode(e.Resolver.Generation(), newPath); ok && dst.Type == "dir" {
		e.Resolver.viewMu.RUnlock()
		return fs.ErrInvalid
	}
	e.Resolver.viewMu.RUnlock()
	unlock, err := e.prepareOverlay(ctx, oldPath)
	if err != nil {
		return err
	}
	defer unlock()
	return e.Overlay.Rename(ctx, oldPath, newPath)
}

func (e *Engine) Mkdir(ctx context.Context, path string, mode uint32) error {
	e.Resolver.viewMu.RLock()
	defer e.Resolver.viewMu.RUnlock()
	return e.Overlay.Mkdir(ctx, path, mode)
}

func (e *Engine) Rmdir(ctx context.Context, path string) error {
	e.Resolver.viewMu.RLock()
	defer e.Resolver.viewMu.RUnlock()
	// Only allow rmdir if the merged directory is empty
	entries, err := e.Resolver.readdirTypedAt(ctx, path, e.Resolver.Generation())
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return os.ErrExist
	}
	return e.Overlay.Remove(ctx, path)
}

// SetMtime promotes base files/directories before updating mtime so the
// caller-controlled timestamp never overwrites base snapshot attrs.
func (e *Engine) SetMtime(ctx context.Context, path string, t time.Time) error {
	e.Resolver.viewMu.RLock()
	path = model.CleanPath(path)
	if path == "." {
		e.Resolver.viewMu.RUnlock()
		return fs.ErrInvalid
	}
	if _, ok := e.Overlay.Get(path); !ok {
		n, err := e.Resolver.resolvePath(path)
		if err != nil {
			e.Resolver.viewMu.RUnlock()
			return err
		}
		switch n.Base.Type {
		case "dir":
			if err := e.Overlay.Mkdir(ctx, path, n.Base.Mode); err != nil {
				e.Resolver.viewMu.RUnlock()
				return err
			}
		case "file":
			e.Resolver.viewMu.RUnlock()
			unlock, err := e.prepareOverlay(ctx, path)
			if err != nil {
				return err
			}
			defer unlock()
			return e.Overlay.SetMtime(ctx, path, t)
		default:
			e.Resolver.viewMu.RUnlock()
			return fs.ErrInvalid
		}
	}
	err := e.Overlay.SetMtime(ctx, path, t)
	e.Resolver.viewMu.RUnlock()
	return err
}

func (e *Engine) Truncate(ctx context.Context, path string, size int64) error {
	e.Resolver.viewMu.RLock()
	if size == 0 {
		if _, ok := e.Overlay.Get(path); !ok {
			n, err := e.Resolver.resolvePath(path)
			if err != nil {
				e.Resolver.viewMu.RUnlock()
				return err
			}
			if !n.FromOverlay {
				if _, err := e.Overlay.CreateTruncated(ctx, path, n.Base); err == nil {
					e.Resolver.viewMu.RUnlock()
					return nil
				} else if !errors.Is(err, os.ErrExist) {
					e.Resolver.viewMu.RUnlock()
					return err
				}
			}
		}
	}
	if _, ok := e.Overlay.Get(path); ok {
		err := e.Overlay.Truncate(ctx, path, size)
		e.Resolver.viewMu.RUnlock()
		return err
	}
	e.Resolver.viewMu.RUnlock()
	unlock, err := e.prepareOverlay(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return e.Overlay.Truncate(ctx, path, size)
}

func (e *Engine) Sync(ctx context.Context, path string) error {
	if _, ok := e.Overlay.Get(path); !ok {
		return nil
	}
	return e.Overlay.SyncFile(ctx, path)
}

// PrefetchDir enqueues file children of a directory for speculative hydration.
// Called from OpenDir in a goroutine so it doesn't block the FUSE operation.
func (e *Engine) PrefetchDir(dirPath string, entries []ReaddirEntry) {
	tasks := make([]model.HydrationTask, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "file" || entry.ObjectOID == "" {
			continue
		}
		childPath := model.CleanPath(filepath.Join(dirPath, entry.Name))
		pri := hydrator.ClassifyPriority(childPath)
		tasks = append(tasks, model.HydrationTask{
			RepoID:     e.Repo.ID,
			Path:       childPath,
			ObjectOID:  entry.ObjectOID,
			SizeState:  entry.SizeState,
			SizeBytes:  entry.SizeBytes,
			Priority:   pri,
			Reason:     "prefetch",
			EnqueuedAt: time.Now(),
		})
	}
	if len(tasks) > maxPrefetchTasksPerDir {
		sort.SliceStable(tasks, func(i, j int) bool {
			if tasks[i].Priority == tasks[j].Priority {
				return tasks[i].Path < tasks[j].Path
			}
			return tasks[i].Priority > tasks[j].Priority
		})
		tasks = tasks[:maxPrefetchTasksPerDir]
	}
	e.Hydrator.EnqueueBatch(tasks)
}

func readFileChunk(path string, off int64, size int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readFileChunkFrom(f, off, size)
}

func readFileChunkFrom(f *os.File, off int64, size int) ([]byte, error) {
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
