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

// ensureOverlay promotes a base file to the overlay (hydrate → copy-on-write).
// No-op if the path already has an overlay entry.
func (e *Engine) ensureOverlay(ctx context.Context, path string) error {
	if _, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return err
	} else if ok {
		return nil
	}
	n, err := e.Resolver.resolvePath(path)
	if err != nil {
		return err
	}
	if n.Base.ObjectOID != "" {
		src, _, hErr := e.Hydrator.OpenHydrated(ctx, e.Repo, n.Base)
		if hErr != nil {
			return hErr
		}
		defer src.Close()
		_, err = e.Overlay.EnsureCopyOnWriteFrom(ctx, e.Repo, path, n.Base, src)
		return err
	}
	_, err = e.Overlay.EnsureCopyOnWrite(ctx, e.Repo, path, n.Base)
	return err
}

func (e *Engine) Read(ctx context.Context, path string, off int64, size int) ([]byte, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return nil, err
	} else if ok {
		if ov.IsDeleted() {
			return nil, os.ErrNotExist
		}
		return readFileChunk(ov.BackingPath, off, size)
	}
	n, err := e.Resolver.resolvePath(path)
	if err != nil {
		return nil, err
	}
	f, _, err := e.Hydrator.OpenHydrated(ctx, e.Repo, n.Base)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readFileChunkFrom(f, off, size)
}

func (e *Engine) BaseCacheFile(ctx context.Context, path string) (*os.File, int64, bool, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if ov, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return nil, 0, false, err
	} else if ok {
		if ov.IsDeleted() {
			return nil, 0, false, os.ErrNotExist
		}
		return nil, 0, false, nil
	}
	gen := e.Resolver.Generation()
	n, ok, err := e.Resolver.Snapshot.LookupNode(ctx, gen, path)
	if err != nil {
		return nil, 0, false, err
	}
	if !ok {
		return nil, 0, false, fs.ErrNotExist
	}
	f, _, err := e.Hydrator.OpenHydrated(ctx, e.Repo, n)
	if err != nil {
		return nil, 0, false, err
	}
	return f, gen, true, nil
}

func (e *Engine) Write(ctx context.Context, path string, off int64, data []byte) (int, error) {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if err := e.ensureOverlay(ctx, path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		// Path doesn't exist in snapshot -- create it
		if _, cErr := e.Overlay.CreateFile(ctx, path, 0o644); cErr != nil {
			return 0, cErr
		}
	}
	return e.Overlay.WriteFile(ctx, path, off, data)
}

func (e *Engine) Sync(ctx context.Context, path string) error {
	return e.Overlay.SyncFile(ctx, path)
}

func (e *Engine) Create(ctx context.Context, path string, mode uint32) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	_, err := e.Overlay.CreateFile(ctx, path, mode)
	return err
}

func (e *Engine) Symlink(ctx context.Context, path string, target string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	_, err := e.Overlay.CreateSymlink(ctx, path, target)
	return err
}

func (e *Engine) Unlink(ctx context.Context, path string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	return e.Overlay.Remove(ctx, path)
}

func (e *Engine) Rename(ctx context.Context, oldPath, newPath string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	if oldPath == newPath {
		_, err := e.Resolver.resolvePath(oldPath)
		return err
	}
	if ov, ok, lookupErr := e.Overlay.Lookup(ctx, oldPath); lookupErr != nil {
		return lookupErr
	} else if ok {
		if ov.IsDeleted() {
			return os.ErrNotExist
		}
		if ov.Kind == model.OverlayKindMkdir {
			if _, ok, err := e.Resolver.Snapshot.LookupNode(ctx, e.Resolver.Generation(), oldPath); err != nil {
				return err
			} else if ok {
				return fs.ErrInvalid
			}
		}
		if dst, ok, err := e.Resolver.Snapshot.LookupNode(ctx, e.Resolver.Generation(), newPath); err != nil {
			return err
		} else if ok {
			if dst.Type == "dir" || ov.Kind == model.OverlayKindMkdir {
				return fs.ErrInvalid
			}
			if ov.Kind == model.OverlayKindCreate || ov.Kind == model.OverlayKindSymlink {
				return e.Overlay.RenameAndMarkModifiedFromBase(ctx, oldPath, newPath, dst.ObjectOID, dst.Mode)
			}
		}
		return e.Overlay.Rename(ctx, oldPath, newPath)
	}
	if n, ok, err := e.Resolver.Snapshot.LookupNode(ctx, e.Resolver.Generation(), oldPath); err != nil {
		return err
	} else if ok && n.Type == "dir" {
		return fs.ErrInvalid
	}
	n, err := e.Resolver.resolvePath(oldPath)
	if err != nil {
		return err
	}
	if n.Base.Type == "dir" {
		return fs.ErrInvalid
	}
	if dst, ok, err := e.Resolver.Snapshot.LookupNode(ctx, e.Resolver.Generation(), newPath); err != nil {
		return err
	} else if ok && dst.Type == "dir" {
		return fs.ErrInvalid
	}
	if err := e.ensureOverlay(ctx, oldPath); err != nil {
		return err
	}
	return e.Overlay.Rename(ctx, oldPath, newPath)
}

func (e *Engine) Mkdir(ctx context.Context, path string, mode uint32) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	return e.Overlay.Mkdir(ctx, path, mode)
}

func (e *Engine) Rmdir(ctx context.Context, path string) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	// Only allow rmdir if the merged directory is empty
	children, err := e.Resolver.readdirTypedAt(ctx, path, e.Resolver.Generation())
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return os.ErrExist
	}
	return e.Overlay.Remove(ctx, path)
}

// SetMtime promotes base files/directories before updating mtime so the
// caller-controlled timestamp never overwrites base snapshot attrs.
func (e *Engine) SetMtime(ctx context.Context, path string, t time.Time) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if path == "." {
		return fs.ErrInvalid
	}
	if _, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return err
	} else if !ok {
		n, err := e.Resolver.resolvePath(path)
		if err != nil {
			return err
		}
		switch n.Base.Type {
		case "dir":
			if err := e.Overlay.Mkdir(ctx, path, n.Base.Mode); err != nil {
				return err
			}
		case "file":
			if err := e.ensureOverlay(ctx, path); err != nil {
				return err
			}
		default:
			return fs.ErrInvalid
		}
	}
	return e.Overlay.SetMtime(ctx, path, t)
}

func (e *Engine) SetMode(ctx context.Context, path string, mode uint32) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	path = model.CleanPath(path)
	if path == "." {
		return fs.ErrInvalid
	}
	if _, ok, err := e.Overlay.Lookup(ctx, path); err != nil {
		return err
	} else if !ok {
		n, err := e.Resolver.resolvePath(path)
		if err != nil {
			return err
		}
		switch n.Base.Type {
		case "dir":
			if err := e.Overlay.Mkdir(ctx, path, n.Base.Mode); err != nil {
				return err
			}
		case "file":
			if err := e.ensureOverlay(ctx, path); err != nil {
				return err
			}
		default:
			return fs.ErrInvalid
		}
	}
	return e.Overlay.SetMode(ctx, path, mode)
}

func (e *Engine) Truncate(ctx context.Context, path string, size int64) error {
	e.Resolver.transition.RLock()
	defer e.Resolver.transition.RUnlock()
	if err := e.ensureOverlay(ctx, path); err != nil {
		return err
	}
	return e.Overlay.Truncate(ctx, path, size)
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
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
