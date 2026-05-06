package fusefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type Engine struct {
	Resolver *Resolver
	Repo     model.RepoConfig
	Overlay  model.OverlayStore
	Hydrator model.Hydrator
}

// ensureOverlay promotes a base file to the overlay (hydrate → copy-on-write).
// No-op if the path already has an overlay entry.
func (e *Engine) ensureOverlay(ctx context.Context, path string) error {
	if _, ok := e.Overlay.Get(path); ok {
		return nil
	}
	n, err := e.Resolver.ResolvePath(path)
	if err != nil {
		return err
	}
	if n.Base.ObjectOID != "" {
		if _, _, hErr := e.Hydrator.EnsureHydrated(ctx, e.Repo, n.Base); hErr != nil {
			return hErr
		}
	}
	_, err = e.Overlay.EnsureCopyOnWrite(ctx, e.Repo, path, n.Base)
	return err
}

func (e *Engine) Read(ctx context.Context, path string, off int64, size int) ([]byte, error) {
	if ov, ok := e.Overlay.Get(path); ok {
		if ov.IsDeleted() {
			return nil, os.ErrNotExist
		}
		return readFileChunk(ov.BackingPath, off, size)
	}
	n, err := e.Resolver.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	cachePath, _, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, n.Base)
	if err != nil {
		return nil, err
	}
	return readFileChunk(cachePath, off, size)
}

func (e *Engine) Write(ctx context.Context, path string, off int64, data []byte) (int, error) {
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

func (e *Engine) Create(ctx context.Context, path string, mode uint32) error {
	_, err := e.Overlay.CreateFile(ctx, path, mode)
	return err
}

func (e *Engine) Unlink(ctx context.Context, path string) error {
	return e.Overlay.Remove(ctx, path)
}

func (e *Engine) Rename(ctx context.Context, oldPath, newPath string) error {
	if _, ok := e.Overlay.Get(oldPath); !ok {
		n, err := e.Resolver.ResolvePath(oldPath)
		if err != nil {
			return err
		}
		if n.Base.Type == "dir" {
			if err := e.Overlay.Mkdir(ctx, oldPath, n.Base.Mode); err != nil {
				return err
			}
		} else if err := e.ensureOverlay(ctx, oldPath); err != nil {
			return err
		}
	}
	return e.Overlay.Rename(ctx, oldPath, newPath)
}

func (e *Engine) Mkdir(ctx context.Context, path string, mode uint32) error {
	return e.Overlay.Mkdir(ctx, path, mode)
}

func (e *Engine) Rmdir(ctx context.Context, path string) error {
	// Only allow rmdir if the merged directory is empty
	children, err := e.Resolver.Readdir(ctx, path)
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
	path = model.CleanPath(path)
	if path == "." {
		return fs.ErrInvalid
	}
	if _, ok := e.Overlay.Get(path); !ok {
		n, err := e.Resolver.ResolvePath(path)
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

func (e *Engine) Truncate(ctx context.Context, path string, size int64) error {
	if err := e.ensureOverlay(ctx, path); err != nil {
		return err
	}
	return e.Overlay.Truncate(ctx, path, size)
}

// PrefetchDir enqueues file children of a directory for speculative hydration.
// Called from OpenDir in a goroutine so it doesn't block the FUSE operation.
func (e *Engine) PrefetchDir(dirPath string, entries []ReaddirEntry) {
	gen := e.Resolver.Generation()
	for _, entry := range entries {
		if entry.Type != "file" {
			continue
		}
		childPath := model.CleanPath(filepath.Join(dirPath, entry.Name))
		n, ok := e.Resolver.Snapshot.GetNode(gen, childPath)
		if !ok || n.ObjectOID == "" {
			continue
		}
		pri := hydrator.ClassifyPriority(childPath)
		e.Hydrator.Enqueue(model.HydrationTask{
			RepoID:     e.Repo.ID,
			Path:       childPath,
			ObjectOID:  n.ObjectOID,
			Priority:   pri,
			Reason:     "prefetch",
			EnqueuedAt: time.Now(),
		})
	}
}

func readFileChunk(path string, off int64, size int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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
