package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS repo_state (
	  key TEXT PRIMARY KEY,
	  value TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS base_nodes (
	  generation INTEGER NOT NULL,
	  path TEXT NOT NULL,
	  parent_path TEXT NOT NULL DEFAULT '',
	  type TEXT NOT NULL,
	  mode INTEGER NOT NULL,
	  object_oid TEXT NOT NULL,
	  size_state TEXT NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  PRIMARY KEY (generation, path)
	);`,
	`CREATE INDEX IF NOT EXISTS idx_base_nodes_gen_parent ON base_nodes(generation, parent_path);`,
	`CREATE TABLE IF NOT EXISTS learned_path_stats (
	  path TEXT PRIMARY KEY,
	  access_count INTEGER NOT NULL DEFAULT 0,
	  last_access_ns INTEGER NOT NULL DEFAULT 0,
	  last_hydrated_ns INTEGER NOT NULL DEFAULT 0
	);`,
	`CREATE TABLE IF NOT EXISTS blob_cache_index (
	  object_oid TEXT PRIMARY KEY,
	  cache_path TEXT NOT NULL,
	  size_bytes INTEGER NOT NULL DEFAULT 0,
	  last_access_ns INTEGER NOT NULL DEFAULT 0
	);`,
}

type Store struct {
	db *sql.DB
}

func New(ctx context.Context, path string) (*Store, error) {
	db, err := meta.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if err := meta.ExecMigrations(ctx, db, migrations); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) PublishGeneration(ctx context.Context, repoID model.RepoID, headOID string, ref string, nodes []model.BaseNode) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	gen, err := s.nextGenerationTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM base_nodes WHERE generation=?`, gen); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO base_nodes(generation, path, parent_path, type, mode, object_oid, size_state, size_bytes) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, n := range nodes {
		parentPath := parentDir(n.Path)
		if _, err := stmt.ExecContext(ctx, gen, n.Path, parentPath, n.Type, n.Mode, n.ObjectOID, n.SizeState, n.SizeBytes); err != nil {
			return 0, err
		}
	}
	if err := upsertState(ctx, tx, "current_generation", fmt.Sprintf("%d", gen)); err != nil {
		return 0, err
	}
	if err := upsertState(ctx, tx, "head_oid", headOID); err != nil {
		return 0, err
	}
	if err := upsertState(ctx, tx, "head_ref", ref); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Clean up old generations to prevent unbounded growth
	if gen > 2 {
		s.db.ExecContext(ctx, `DELETE FROM base_nodes WHERE generation < ?`, gen-1)
	}
	return gen, nil
}

func (s *Store) CurrentGeneration(ctx context.Context) (int64, error) {
	row := s.db.QueryRowContext(ctx, `SELECT value FROM repo_state WHERE key='current_generation'`)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	var gen int64
	_, err := fmt.Sscanf(val, "%d", &gen)
	return gen, err
}

func (s *Store) GetNode(_ model.RepoID, generation int64, path string) (model.BaseNode, bool) {
	// Uses background context for backward compat; callers with a deadline
	// should use GetNodeCtx.
	return s.GetNodeCtx(context.Background(), generation, path)
}

func (s *Store) GetNodeCtx(ctx context.Context, generation int64, path string) (model.BaseNode, bool) {
	row := s.db.QueryRowContext(ctx, `SELECT path, type, mode, object_oid, size_state, size_bytes FROM base_nodes WHERE generation=? AND path=?`, generation, path)
	var n model.BaseNode
	if err := row.Scan(&n.Path, &n.Type, &n.Mode, &n.ObjectOID, &n.SizeState, &n.SizeBytes); err != nil {
		return model.BaseNode{}, false
	}
	return n, true
}

// ListChildren returns direct children of parentPath using a path-based lookup
// (no inode join, no collision risk).
func (s *Store) ListChildren(_ model.RepoID, generation int64, parentPath string) ([]model.BaseNode, error) {
	pp := model.CleanPath(parentPath)
	rows, err := s.db.Query(`SELECT path, type, mode, object_oid, size_state, size_bytes FROM base_nodes WHERE generation=? AND parent_path=? ORDER BY path`, generation, pp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BaseNode
	for rows.Next() {
		var n model.BaseNode
		if err := rows.Scan(&n.Path, &n.Type, &n.Mode, &n.ObjectOID, &n.SizeState, &n.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) nextGenerationTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	row := tx.QueryRowContext(ctx, `SELECT value FROM repo_state WHERE key='current_generation'`)
	var val string
	if err := row.Scan(&val); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1, nil
		}
		return 0, err
	}
	var gen int64
	if _, err := fmt.Sscanf(val, "%d", &gen); err != nil {
		return 0, err
	}
	return gen + 1, nil
}

func upsertState(ctx context.Context, tx *sql.Tx, key string, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO repo_state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// parentDir returns the parent path for a given path, using "." as the root parent.
func parentDir(path string) string {
	if path == "." || path == "/" || path == "" {
		return ""
	}
	d := filepath.Dir(path)
	if d == "/" {
		return "."
	}
	return d
}
