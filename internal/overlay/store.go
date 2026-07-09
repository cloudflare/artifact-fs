package overlay

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS overlay_entries (
	  path TEXT PRIMARY KEY,
	  kind TEXT NOT NULL,
	  backing_path TEXT,
	  mode INTEGER NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  mtime_unix_ns INTEGER NOT NULL,
	  ctime_unix_ns INTEGER NOT NULL,
	  source_oid TEXT,
	  target_path TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_overlay_kind ON overlay_entries(kind);`,
}

type Store struct {
	db         *sql.DB
	repo       model.RepoConfig
	upperDir   string
	mutationMu sync.RWMutex
	pathLocks  [256]sync.Mutex
	mu         sync.RWMutex
}

func New(ctx context.Context, cfg model.RepoConfig) (*Store, error) {
	db, err := meta.OpenDB(cfg.OverlayDBPath)
	if err != nil {
		return nil, err
	}
	if err := meta.ExecMigrations(ctx, db, migrations); err != nil {
		return nil, err
	}
	if err := ensureOverlaySchema(ctx, db); err != nil {
		return nil, err
	}
	upperDir := filepath.Join(cfg.OverlayDir, "upper")
	if err := os.MkdirAll(upperDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{db: db, repo: cfg, upperDir: upperDir}, nil
}

func OpenReadOnly(cfg model.RepoConfig) (*Store, error) {
	db, err := meta.OpenDBReadOnly(cfg.OverlayDBPath)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, repo: cfg, upperDir: filepath.Join(cfg.OverlayDir, "upper")}, nil
}

func ensureOverlaySchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(overlay_entries)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasCtime := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "ctime_unix_ns" {
			hasCtime = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasCtime {
		if _, err := db.ExecContext(ctx, `ALTER TABLE overlay_entries ADD COLUMN ctime_unix_ns INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE overlay_entries SET ctime_unix_ns=? WHERE ctime_unix_ns=0`, time.Now().UnixNano())
	return err
}

func (s *Store) Close() error {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// overlayCols is the column list for overlay_entries queries. Keep in sync with
// the Scan call in queryEntries and the single-row scan in Get.
const overlayCols = `path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path`

func (s *Store) Get(path string) (model.OverlayEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.get(path)
}

func (s *Store) get(path string) (model.OverlayEntry, bool) {
	row := s.db.QueryRow(`SELECT `+overlayCols+` FROM overlay_entries WHERE path=?`, model.CleanPath(path))
	var e model.OverlayEntry
	if err := row.Scan(&e.Path, &e.Kind, &e.BackingPath, &e.Mode, &e.SizeBytes, &e.MtimeUnixNs, &e.CtimeUnixNs, &e.SourceOID, &e.TargetPath); err != nil {
		return model.OverlayEntry{}, false
	}
	e.RepoID = s.repo.ID
	return e, true
}

// EnsureCopyOnWrite promotes a base file into the overlay. If the blob is not
// cached, an empty overlay file is created and the caller must hydrate first.
func (s *Store) EnsureCopyOnWrite(ctx context.Context, repo model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	if e, ok := s.Get(path); ok && !e.IsDeleted() {
		return e, nil
	}
	backing := s.backingPath(path)
	if err := os.MkdirAll(filepath.Dir(backing), 0o755); err != nil {
		return model.OverlayEntry{}, err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(backing), ".artifact-fs-cow-*")
	if err != nil {
		return model.OverlayEntry{}, err
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return model.OverlayEntry{}, err
	}
	defer os.Remove(tmp)
	if err := os.Chmod(tmp, os.FileMode(base.Mode)); err != nil {
		return model.OverlayEntry{}, err
	}
	if base.ObjectOID != "" {
		cachePath := filepath.Join(repo.BlobCacheDir, base.ObjectOID)
		if err := copyFileContents(cachePath, tmp, os.FileMode(base.Mode)); err != nil && !os.IsNotExist(err) {
			return model.OverlayEntry{}, fmt.Errorf("copy-on-write %s: %w", path, err)
		}
		// If the cache file doesn't exist yet, the overlay starts empty and
		// the caller must hydrate before writing.
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.get(path); ok && !e.IsDeleted() {
		return e, nil
	}
	if err := os.Rename(tmp, backing); err != nil {
		return model.OverlayEntry{}, err
	}
	st, err := os.Stat(backing)
	if err != nil {
		return model.OverlayEntry{}, err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{
		RepoID:      s.repo.ID,
		Path:        model.CleanPath(path),
		Kind:        model.OverlayKindModify,
		BackingPath: backing,
		Mode:        base.Mode,
		SizeBytes:   st.Size(),
		MtimeUnixNs: now,
		CtimeUnixNs: now,
		SourceOID:   base.ObjectOID,
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		_ = os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	return e, nil
}

func (s *Store) CreateFile(ctx context.Context, path string, mode uint32) (model.OverlayEntry, error) {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	if e, ok := s.get(path); ok && !e.IsDeleted() {
		return model.OverlayEntry{}, os.ErrExist
	}
	backing := s.backingPath(path)
	if err := os.Remove(backing); err != nil && !errors.Is(err, os.ErrNotExist) {
		return model.OverlayEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(backing), 0o755); err != nil {
		return model.OverlayEntry{}, err
	}
	f, err := os.OpenFile(backing, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode))
	if err != nil {
		return model.OverlayEntry{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: path, Kind: model.OverlayKindCreate, BackingPath: backing, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	if err := s.upsertEntry(ctx, e); err != nil {
		_ = os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	return e, nil
}

// CreateTruncated promotes a base file directly to an empty overlay file. It
// avoids hydrating and copying bytes that truncate(0) will immediately discard.
func (s *Store) CreateTruncated(ctx context.Context, path string, base model.BaseNode) (model.OverlayEntry, error) {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	if e, ok := s.get(path); ok && !e.IsDeleted() {
		return model.OverlayEntry{}, os.ErrExist
	}
	backing := s.backingPath(path)
	if err := os.Remove(backing); err != nil && !errors.Is(err, os.ErrNotExist) {
		return model.OverlayEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(backing), 0o755); err != nil {
		return model.OverlayEntry{}, err
	}
	f, err := os.OpenFile(backing, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(base.Mode))
	if err != nil {
		return model.OverlayEntry{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{
		RepoID:      s.repo.ID,
		Path:        path,
		Kind:        model.OverlayKindModify,
		BackingPath: backing,
		Mode:        base.Mode,
		MtimeUnixNs: now,
		CtimeUnixNs: now,
		SourceOID:   base.ObjectOID,
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		_ = os.Remove(backing)
		return model.OverlayEntry{}, err
	}
	return e, nil
}

func (s *Store) WriteFile(ctx context.Context, path string, off int64, data []byte) (int, error) {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.get(path)
	if !ok || e.IsDeleted() {
		return 0, os.ErrNotExist
	}
	f, err := os.OpenFile(e.BackingPath, os.O_WRONLY, os.FileMode(e.Mode))
	if err != nil {
		return 0, err
	}
	n, err := f.WriteAt(data, off)
	if err != nil {
		_ = f.Close()
		return n, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	now := time.Now().UnixNano()
	e.SizeBytes = st.Size()
	e.MtimeUnixNs = now
	e.CtimeUnixNs = now
	if e.Kind != model.OverlayKindCreate {
		e.Kind = model.OverlayKindModify
	}
	return n, s.upsertEntry(ctx, e)
}

func (s *Store) Truncate(ctx context.Context, path string, size int64) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.get(path)
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	previous := e
	now := time.Now().UnixNano()
	e.SizeBytes = size
	e.MtimeUnixNs = now
	e.CtimeUnixNs = now
	if e.Kind != model.OverlayKindCreate {
		e.Kind = model.OverlayKindModify
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		return err
	}
	if err := os.Truncate(e.BackingPath, size); err != nil {
		if restoreErr := s.upsertEntry(ctx, previous); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore truncate metadata: %w", restoreErr))
		}
		return err
	}
	return nil
}

func (s *Store) Remove(ctx context.Context, path string) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	previous, existed := s.get(path)
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: path, Kind: model.OverlayKindDelete, Mode: 0, MtimeUnixNs: now, CtimeUnixNs: now}
	if err := s.upsertEntry(ctx, e); err != nil {
		return err
	}
	if existed && previous.BackingPath != "" {
		_ = os.Remove(previous.BackingPath)
	}
	return nil
}

func (s *Store) Rename(ctx context.Context, oldPath, newPath string) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPaths := s.lockPaths(oldPath, newPath)
	defer unlockPaths()
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e, ok := s.get(oldPath)
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	if oldPath == newPath {
		return nil
	}
	destination, destinationExists := s.get(newPath)
	newBacking := s.backingPath(newPath)
	if err := os.MkdirAll(filepath.Dir(newBacking), 0o755); err != nil {
		return err
	}
	newKind := model.OverlayKindRename
	newTargetPath := oldPath
	writeSourceWhiteout := true
	if e.Kind == model.OverlayKindRename && e.TargetPath != "" {
		newTargetPath = e.TargetPath
	}
	mode := e.Mode
	normalizedDirMode := false
	var deletedDescendants []model.OverlayEntry
	switch e.Kind {
	case model.OverlayKindCreate, model.OverlayKindSymlink:
		newKind = e.Kind
		newTargetPath = ""
		writeSourceWhiteout = false
	case model.OverlayKindMkdir:
		hasDescendants, err := s.hasOverlayDescendants(ctx, oldPath)
		if err != nil {
			return err
		}
		if hasDescendants {
			return iofs.ErrInvalid
		}
		deletedDescendants, err = s.deletedDescendants(ctx, oldPath)
		if err != nil {
			return err
		}
		newKind = model.OverlayKindMkdir
		newTargetPath = ""
		writeSourceWhiteout = false
		if normalizedMode, normalized := normalizeGitDirMode(mode); normalized {
			mode = normalizedMode
			normalizedDirMode = true
		}
	}
	// DB transaction first, then filesystem rename. If the DB commit fails,
	// nothing has moved on disk and the overlay remains consistent.
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path IN (?,?)`, oldPath, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path) VALUES(?,?,?,?,?,?,?,?,?)`, newPath, newKind, newBacking, mode, e.SizeBytes, e.MtimeUnixNs, now, e.SourceOID, newTargetPath); err != nil {
		return err
	}
	if e.Kind == model.OverlayKindMkdir {
		prefix := oldPath + "/"
		if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE kind=? AND substr(path, 1, length(?))=?`, model.OverlayKindDelete, prefix, prefix); err != nil {
			return err
		}
	}
	if writeSourceWhiteout {
		if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET kind='delete',mtime_unix_ns=excluded.mtime_unix_ns,ctime_unix_ns=excluded.ctime_unix_ns,target_path=excluded.target_path`, oldPath, model.OverlayKindDelete, "", 0, 0, now, now, "", newTargetPath); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Filesystem rename after successful commit.
	if e.BackingPath != "" {
		if err := os.Rename(e.BackingPath, newBacking); err != nil {
			restoreErr := s.restoreRenameEntries(ctx, newPath, e, destination, destinationExists, deletedDescendants)
			return errors.Join(err, restoreErr)
		}
		if normalizedDirMode {
			if err := os.Chmod(newBacking, os.FileMode(mode)); err != nil {
				restoreErr := s.restoreRenameEntries(ctx, newPath, e, destination, destinationExists, deletedDescendants)
				renameErr := os.Rename(newBacking, e.BackingPath)
				return errors.Join(err, restoreErr, renameErr)
			}
		}
	}
	return nil
}

func (s *Store) deletedDescendants(ctx context.Context, path string) ([]model.OverlayEntry, error) {
	prefix := model.CleanPath(path) + "/"
	return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries WHERE kind=? AND substr(path, 1, length(?))=? ORDER BY path`, model.OverlayKindDelete, prefix, prefix)
}

func (s *Store) RenameAndMarkModifiedFromBase(ctx context.Context, oldPath, newPath string, sourceOID string) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPaths := s.lockPaths(oldPath, newPath)
	defer unlockPaths()
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e, ok := s.get(oldPath)
	if !ok || e.IsDeleted() {
		return os.ErrNotExist
	}
	if oldPath == newPath {
		_, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET kind=?, source_oid=?, target_path='' WHERE path=?`, model.OverlayKindModify, sourceOID, oldPath)
		return err
	}
	destination, destinationExists := s.get(newPath)
	newBacking := s.backingPath(newPath)
	if err := os.MkdirAll(filepath.Dir(newBacking), 0o755); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path IN (?,?)`, oldPath, newPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path) VALUES(?,?,?,?,?,?,?,?,?)`, newPath, model.OverlayKindModify, newBacking, e.Mode, e.SizeBytes, e.MtimeUnixNs, now, sourceOID, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if e.BackingPath != "" {
		if err := os.Rename(e.BackingPath, newBacking); err != nil {
			restoreErr := s.restoreRenameEntries(ctx, newPath, e, destination, destinationExists, nil)
			return errors.Join(err, restoreErr)
		}
	}
	return nil
}

func (s *Store) restoreRenameEntries(ctx context.Context, newPath string, source model.OverlayEntry, destination model.OverlayEntry, destinationExists bool, deletedDescendants []model.OverlayEntry) error {
	tx, err := s.db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM overlay_entries WHERE path=?`, newPath); err != nil {
		return err
	}
	insert := func(e model.OverlayEntry) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path) VALUES(?,?,?,?,?,?,?,?,?)`, e.Path, e.Kind, e.BackingPath, e.Mode, e.SizeBytes, e.MtimeUnixNs, e.CtimeUnixNs, e.SourceOID, e.TargetPath)
		return err
	}
	if err := insert(source); err != nil {
		return err
	}
	if destinationExists {
		if err := insert(destination); err != nil {
			return err
		}
	}
	for _, descendant := range deletedDescendants {
		if err := insert(descendant); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) hasOverlayDescendants(ctx context.Context, path string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM overlay_entries WHERE kind <> ?`, model.OverlayKindDelete)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	prefix := path + "/"
	for rows.Next() {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return false, err
		}
		if strings.HasPrefix(candidate, prefix) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Mkdir(ctx context.Context, path string, mode uint32) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	mode, normalized := normalizeGitDirMode(mode)
	backing := s.backingPath(path)
	if err := os.MkdirAll(backing, os.FileMode(mode)); err != nil {
		return err
	}
	if normalized {
		if err := os.Chmod(backing, os.FileMode(mode)); err != nil {
			return err
		}
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: path, Kind: model.OverlayKindMkdir, BackingPath: backing, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	return s.upsertEntry(ctx, e)
}

func normalizeGitDirMode(mode uint32) (uint32, bool) {
	if mode&0o170000 == 0o40000 && mode&0o777 == 0 {
		return 0o755, true
	}
	return mode, false
}

func (s *Store) SetMtime(ctx context.Context, path string, t time.Time) error {
	s.mutationMu.RLock()
	defer s.mutationMu.RUnlock()
	unlockPath := s.lockPaths(path)
	defer unlockPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	path = model.CleanPath(path)
	if e, ok := s.get(path); ok && e.Kind == model.OverlayKindMkdir {
		if mode, normalized := normalizeGitDirMode(e.Mode); normalized {
			if err := os.Chmod(e.BackingPath, os.FileMode(mode)); err != nil {
				return err
			}
			_, err := s.db.ExecContext(ctx,
				`UPDATE overlay_entries SET mode=?, mtime_unix_ns=?, ctime_unix_ns=? WHERE path=?`,
				mode, t.UnixNano(), time.Now().UnixNano(), path)
			return err
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE overlay_entries SET mtime_unix_ns=?, ctime_unix_ns=? WHERE path=?`,
		t.UnixNano(), time.Now().UnixNano(), path)
	return err
}

func (s *Store) SyncFile(_ context.Context, path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.get(path)
	if !ok || e.IsDeleted() || e.BackingPath == "" {
		return os.ErrNotExist
	}
	f, err := os.Open(e.BackingPath)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Reconcile prunes overlay entries that are stale relative to the new base
// snapshot. Called after a generation change (commit, branch switch, fetch).
//
// Rules for each overlay entry:
//   - modify with source_oid matching the base OID at path: base unchanged, KEEP
//   - rename with source_oid matching the base OID at original path: base unchanged, KEEP
//   - modify/rename with source_oid mismatch: base changed, REMOVE
//   - create where base now has the path: was committed or exists on new branch, REMOVE
//   - create where base doesn't have the path: user-created, KEEP
//   - delete (whiteout) where base doesn't have the path: irrelevant, REMOVE
//   - delete (whiteout) where base has the path: still meaningful, KEEP
//   - mkdir where base now has a dir at the path: REMOVE
//   - mkdir where base doesn't have the path: user-created, KEEP
func (s *Store) Reconcile(ctx context.Context, baseLookup func(path string) (model.BaseNode, bool)) error {
	return s.reconcile(ctx, baseLookup, nil, nil)
}

func (s *Store) ReconcileAndPublish(ctx context.Context, baseLookup func(path string) (model.BaseNode, bool), beginPublish func() func(), publish func()) error {
	return s.reconcile(ctx, baseLookup, beginPublish, publish)
}

func (s *Store) reconcile(ctx context.Context, baseLookup func(path string) (model.BaseNode, bool), beginPublish func() func(), publish func()) error {
	if baseLookup == nil {
		return nil
	}
	s.mu.RLock()
	entries, err := s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries ORDER BY path`)
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("reconcile list: %w", err)
	}
	var toRemove []model.OverlayEntry
	type entryUpdate struct {
		before model.OverlayEntry
		after  model.OverlayEntry
	}
	var toUpdate []entryUpdate
	staleRenameSources := map[string]bool{}
	type baseResult struct {
		node model.BaseNode
		ok   bool
	}
	baseCache := make(map[string]baseResult, len(entries))
	lookupBase := func(path string) (model.BaseNode, bool) {
		if result, ok := baseCache[path]; ok {
			return result.node, result.ok
		}
		node, ok := baseLookup(path)
		baseCache[path] = baseResult{node: node, ok: ok}
		return node, ok
	}
	for _, e := range entries {
		base, baseExists := lookupBase(e.Path)
		switch e.Kind {
		case model.OverlayKindModify:
			if !baseExists {
				updated := e
				updated.Kind = model.OverlayKindCreate
				updated.SourceOID = ""
				toUpdate = append(toUpdate, entryUpdate{before: e, after: updated})
			} else if e.SourceOID != base.ObjectOID {
				matches, err := backingMatchesBase(e, base)
				if err != nil {
					return fmt.Errorf("reconcile hash %s: %w", e.Path, err)
				}
				if matches {
					toRemove = append(toRemove, e)
				} else {
					updated := e
					updated.SourceOID = base.ObjectOID
					toUpdate = append(toUpdate, entryUpdate{before: e, after: updated})
				}
			}
		case model.OverlayKindRename:
			sourcePath := e.TargetPath
			if sourcePath == "" {
				sourcePath = e.Path
			}
			sourceBase, sourceExists := lookupBase(sourcePath)
			if !sourceExists || e.SourceOID != sourceBase.ObjectOID {
				toRemove = append(toRemove, e)
				staleRenameSources[sourcePath] = true
			}
		}
	}
	for _, e := range entries {
		base, baseExists := lookupBase(e.Path)
		switch e.Kind {
		case model.OverlayKindDelete:
			if staleRenameSources[e.Path] || staleRenameSources[e.TargetPath] || !baseExists {
				toRemove = append(toRemove, e)
			}
		case model.OverlayKindCreate:
			if baseExists {
				matches, err := backingMatchesBase(e, base)
				if err != nil {
					return fmt.Errorf("reconcile hash %s: %w", e.Path, err)
				}
				if matches {
					toRemove = append(toRemove, e)
				} else {
					updated := e
					updated.Kind = model.OverlayKindModify
					updated.SourceOID = base.ObjectOID
					toUpdate = append(toUpdate, entryUpdate{before: e, after: updated})
				}
			}
		case model.OverlayKindMkdir:
			if baseExists && base.Type == "dir" {
				toRemove = append(toRemove, e)
			}
		}
	}
	finishPublish := func() {}
	if beginPublish != nil {
		finishPublish = beginPublish()
	}
	defer finishPublish()
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(toRemove) == 0 && len(toUpdate) == 0 {
		if publish != nil {
			publish()
		}
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Guard the DELETE so a concurrent FUSE write between our read and this
	// delete is not lost. source_oid protects modify/rename entries (writes
	// change the OID). mtime_unix_ns protects create/delete entries where
	// source_oid is always empty -- any concurrent write updates the mtime.
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM overlay_entries WHERE path=? AND kind=? AND source_oid=? AND mtime_unix_ns=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	deleted := make([]model.OverlayEntry, 0, len(toRemove))
	for _, e := range toRemove {
		res, err := stmt.ExecContext(ctx, e.Path, e.Kind, e.SourceOID, e.MtimeUnixNs)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			deleted = append(deleted, e)
		}
	}
	updateStmt, err := tx.PrepareContext(ctx, `UPDATE overlay_entries SET kind=?, source_oid=?, target_path=? WHERE path=? AND kind=? AND source_oid=? AND mtime_unix_ns=?`)
	if err != nil {
		return err
	}
	defer updateStmt.Close()
	for _, update := range toUpdate {
		if _, err := updateStmt.ExecContext(ctx,
			update.after.Kind, update.after.SourceOID, update.after.TargetPath,
			update.before.Path, update.before.Kind, update.before.SourceOID, update.before.MtimeUnixNs,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Delete backing files only after the transaction commits. If we deleted
	// before commit and the transaction rolled back, DB rows would reference
	// non-existent files. Reverse order so children are removed before parents
	// (os.Remove fails on non-empty directories).
	for _, v := range slices.Backward(deleted) {
		if v.BackingPath != "" {
			_ = os.Remove(v.BackingPath)
		}
	}
	if publish != nil {
		publish()
	}
	return nil
}

func backingMatchesOID(path, oid string) (bool, error) {
	if path == "" || oid == "" {
		return false, nil
	}
	var digest hash.Hash
	switch len(oid) {
	case sha1.Size * 2:
		digest = sha1.New()
	case sha256.Size * 2:
		digest = sha256.New()
	default:
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if _, err := io.WriteString(digest, "blob "+strconv.FormatInt(info.Size(), 10)+"\x00"); err != nil {
		return false, err
	}
	if _, err := io.Copy(digest, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(digest.Sum(nil)) == oid, nil
}

func backingMatchesBase(entry model.OverlayEntry, base model.BaseNode) (bool, error) {
	if base.Type != "file" || entry.Mode&0o111 != base.Mode&0o111 {
		return false, nil
	}
	return backingMatchesOID(entry.BackingPath, base.ObjectOID)
}

func (s *Store) DirtyCount(ctx context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind <> ?`, model.OverlayKindDelete)
	var c int64
	err := row.Scan(&c)
	return c, err
}

func (s *Store) ListAll(ctx context.Context) ([]model.OverlayEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries ORDER BY path`)
}

// ListByPrefix returns descendants of prefix. Readdir uses them to preserve
// implicit dirty directories when a branch switch removes their base parent.
func (s *Store) ListByPrefix(ctx context.Context, prefix string) ([]model.OverlayEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix = model.CleanPath(prefix)
	if prefix == "." {
		return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries ORDER BY path`)
	}
	lower := prefix + "/"
	upper := prefix + "0"
	return s.queryEntries(ctx, `SELECT `+overlayCols+` FROM overlay_entries WHERE path>=? AND path<? ORDER BY path`, lower, upper)
}

func (s *Store) HasDescendant(ctx context.Context, prefix string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix = model.CleanPath(prefix)
	var row *sql.Row
	if prefix == "." {
		row = s.db.QueryRowContext(ctx, `SELECT 1 FROM overlay_entries WHERE kind<>? LIMIT 1`, model.OverlayKindDelete)
	} else {
		lower := prefix + "/"
		upper := prefix + "0"
		row = s.db.QueryRowContext(ctx, `SELECT 1 FROM overlay_entries WHERE path>=? AND path<? AND kind<>? LIMIT 1`, lower, upper, model.OverlayKindDelete)
	}
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// queryEntries runs an arbitrary overlay query and scans the results.
func (s *Store) queryEntries(ctx context.Context, query string, args ...any) ([]model.OverlayEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OverlayEntry
	for rows.Next() {
		var e model.OverlayEntry
		if err := rows.Scan(&e.Path, &e.Kind, &e.BackingPath, &e.Mode, &e.SizeBytes, &e.MtimeUnixNs, &e.CtimeUnixNs, &e.SourceOID, &e.TargetPath); err != nil {
			return nil, err
		}
		e.RepoID = s.repo.ID
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) upsertEntry(ctx context.Context, e model.OverlayEntry) error {
	if e.Path == "" {
		return errors.New("empty path")
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO overlay_entries(path, kind, backing_path, mode, size_bytes, mtime_unix_ns, ctime_unix_ns, source_oid, target_path)
	VALUES(?,?,?,?,?,?,?,?,?)
	ON CONFLICT(path) DO UPDATE SET
	kind=excluded.kind,
	backing_path=excluded.backing_path,
	mode=excluded.mode,
	size_bytes=excluded.size_bytes,
	mtime_unix_ns=excluded.mtime_unix_ns,
	ctime_unix_ns=excluded.ctime_unix_ns,
	source_oid=excluded.source_oid,
	target_path=excluded.target_path`, e.Path, e.Kind, e.BackingPath, e.Mode, e.SizeBytes, e.MtimeUnixNs, e.CtimeUnixNs, e.SourceOID, e.TargetPath)
	return err
}

// copyFileContents copies src into dst, truncating dst first. Returns
// os.ErrNotExist if src doesn't exist so the caller can decide whether that's
// fatal. Other errors (permissions, I/O) are returned as-is.
func copyFileContents(src string, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tf, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(tf, f); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	return tf.Close()
}

func (s *Store) backingPath(path string) string {
	clean := model.CleanPath(path)
	return filepath.Join(s.upperDir, clean)
}

func (s *Store) lockPaths(paths ...string) func() {
	indices := make([]int, 0, len(paths))
	seen := make(map[int]struct{}, len(paths))
	for _, path := range paths {
		const offset32 = uint32(2166136261)
		const prime32 = uint32(16777619)
		hash := offset32
		clean := model.CleanPath(path)
		for i := range len(clean) {
			hash ^= uint32(clean[i])
			hash *= prime32
		}
		index := int(hash % uint32(len(s.pathLocks)))
		if _, ok := seen[index]; !ok {
			seen[index] = struct{}{}
			indices = append(indices, index)
		}
	}
	slices.Sort(indices)
	for _, index := range indices {
		s.pathLocks[index].Lock()
	}
	return func() {
		for i := len(indices) - 1; i >= 0; i-- {
			s.pathLocks[indices[i]].Unlock()
		}
	}
}

func (s *Store) String() string {
	return fmt.Sprintf("overlay[%s]", s.repo.Name)
}
