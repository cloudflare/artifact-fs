package fusefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

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
		if _, _, hErr := e.Hydrator.EnsureHydrated(ctx, e.Repo, path, n.Base.ObjectOID); hErr != nil {
			return hErr
		}
	}
	_, err = e.Overlay.EnsureCopyOnWrite(ctx, e.Repo, path, n.Base)
	return err
}

func (e *Engine) Read(ctx context.Context, path string, off int64, size int) ([]byte, error) {
	if e.Overlay.HasWhiteout(path) {
		return nil, os.ErrNotExist
	}
	if ov, ok := e.Overlay.Get(path); ok && ov.Kind != "delete" {
		return readFileChunk(ov.BackingPath, off, size)
	}
	n, err := e.Resolver.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	cachePath, _, err := e.Hydrator.EnsureHydrated(ctx, e.Repo, path, n.Base.ObjectOID)
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

// SetMtime updates the mtime for a path in the overlay. No-op for base files
// not yet promoted to the overlay (their mtime comes from the generation epoch).
func (e *Engine) SetMtime(ctx context.Context, path string, t time.Time) {
	_ = e.Overlay.SetMtime(ctx, path, t)
}

func (e *Engine) Truncate(ctx context.Context, path string, size int64) error {
	if err := e.ensureOverlay(ctx, path); err != nil {
		return err
	}
	ov, ok := e.Overlay.Get(path)
	if !ok {
		return os.ErrNotExist
	}
	return os.Truncate(ov.BackingPath, size)
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
