package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var ErrRepoChanged = errors.New("repo config changed")

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS repos (
	  repo_id TEXT PRIMARY KEY,
	  name TEXT NOT NULL UNIQUE,
	  mount_root TEXT NOT NULL,
	  mount_path TEXT NOT NULL,
	  remote_url TEXT NOT NULL DEFAULT '',
	  remote_url_redacted TEXT NOT NULL,
	  remote_url_secret_ref TEXT,
	  branch TEXT NOT NULL,
	  refresh_interval_seconds INTEGER NOT NULL,
	  refresh_interval_ns INTEGER NOT NULL DEFAULT 0,
	  git_dir TEXT NOT NULL,
	  overlay_dir TEXT NOT NULL,
	  blob_cache_dir TEXT NOT NULL,
	  meta_db_path TEXT NOT NULL,
	  overlay_db_path TEXT NOT NULL,
	  enabled INTEGER NOT NULL DEFAULT 1,
	  prepared_gitdir INTEGER NOT NULL DEFAULT 0,
	  fetch_ref TEXT NOT NULL DEFAULT '',
	  prepare_state TEXT NOT NULL DEFAULT '',
	  prepare_error TEXT NOT NULL DEFAULT '',
	  mode TEXT NOT NULL DEFAULT 'workspace',
	  expected_oid TEXT NOT NULL DEFAULT '',
	  created_at_ns INTEGER NOT NULL,
	  updated_at_ns INTEGER NOT NULL
	);`,
}

type Store struct {
	db *sql.DB
}

func New(ctx context.Context, dbPath string) (*Store, error) {
	db, err := meta.OpenDB(dbPath)
	if err != nil {
		return nil, err
	}
	if err := meta.ExecMigrations(ctx, db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureRepoColumns(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
	if cfg.Mode == "" {
		cfg.Mode = model.RepoModeWorkspace
	}
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO repos (repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, fetch_ref, prepare_state, prepare_error, mode, expected_oid, created_at_ns, updated_at_ns)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(repo_id) DO UPDATE SET
	name=excluded.name,
	mount_root=excluded.mount_root,
	mount_path=excluded.mount_path,
	remote_url=excluded.remote_url,
	remote_url_redacted=excluded.remote_url_redacted,
	branch=excluded.branch,
	refresh_interval_seconds=excluded.refresh_interval_seconds,
	refresh_interval_ns=excluded.refresh_interval_ns,
	git_dir=excluded.git_dir,
	overlay_dir=excluded.overlay_dir,
	blob_cache_dir=excluded.blob_cache_dir,
	meta_db_path=excluded.meta_db_path,
	overlay_db_path=excluded.overlay_db_path,
	enabled=excluded.enabled,
	prepared_gitdir=excluded.prepared_gitdir,
	fetch_ref=excluded.fetch_ref,
	prepare_state=excluded.prepare_state,
	prepare_error=excluded.prepare_error,
	mode=excluded.mode,
	expected_oid=excluded.expected_oid,
	updated_at_ns=excluded.updated_at_ns
	`, string(cfg.ID), cfg.Name, cfg.MountRoot, cfg.MountPath, cfg.RemoteURL, cfg.RemoteURLRedacted, cfg.Branch, int64(cfg.RefreshInterval.Seconds()), int64(cfg.RefreshInterval), cfg.GitDir, cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath, cfg.OverlayDBPath, boolToInt(cfg.Enabled), boolToInt(cfg.PreparedGitDir), cfg.FetchRef, cfg.PrepareState, cfg.PrepareError, cfg.Mode, cfg.ExpectedOID, now, now)
	return err
}

func (s *Store) UpdateRefresh(ctx context.Context, repoID model.RepoID, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("refresh interval must be positive")
	}
	res, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET refresh_interval_seconds=?, refresh_interval_ns=?, updated_at_ns=?
	WHERE repo_id=?
	`, int64(interval.Seconds()), int64(interval), time.Now().UnixNano(), string(repoID))
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("repo not found")
	}
	return nil
}

func (s *Store) UpdatePrepareState(ctx context.Context, repoID model.RepoID, state string, prepareErr string) error {
	_, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET prepare_state=?, prepare_error=?, updated_at_ns=?
	WHERE repo_id=?
	`, state, prepareErr, time.Now().UnixNano(), string(repoID))
	return err
}

func (s *Store) UpdatePrepareStateForConfig(ctx context.Context, cfg model.RepoConfig, state string, prepareErr string) error {
	res, err := s.db.ExecContext(ctx, `
	UPDATE repos
	SET prepare_state=?, prepare_error=?, updated_at_ns=?
	WHERE repo_id=?
	  AND branch=?
	  AND remote_url=?
	  AND prepared_gitdir=?
	  AND fetch_ref=?
	  AND git_dir=?
	  AND overlay_dir=?
	  AND blob_cache_dir=?
	  AND meta_db_path=?
	  AND overlay_db_path=?
	  AND mount_path=?
	  AND mode=?
	  AND expected_oid=?
	`, state, prepareErr, time.Now().UnixNano(), string(cfg.ID), cfg.Branch, cfg.RemoteURL, boolToInt(cfg.PreparedGitDir), cfg.FetchRef, cfg.GitDir, cfg.OverlayDir, cfg.BlobCacheDir, cfg.MetaDBPath, cfg.OverlayDBPath, cfg.MountPath, cfg.Mode, cfg.ExpectedOID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrRepoChanged
	}
	return nil
}

func (s *Store) RemoveRepo(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE name=?`, name)
	return err
}

func (s *Store) GetRepo(ctx context.Context, name string) (model.RepoConfig, error) {
	row := s.db.QueryRowContext(ctx, `SELECT repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, fetch_ref, prepare_state, prepare_error, mode, expected_oid FROM repos WHERE name=?`, name)
	return scanRepo(row)
}

func (s *Store) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, name, mount_root, mount_path, remote_url, remote_url_redacted, branch, refresh_interval_seconds, refresh_interval_ns, git_dir, overlay_dir, blob_cache_dir, meta_db_path, overlay_db_path, enabled, prepared_gitdir, fetch_ref, prepare_state, prepare_error, mode, expected_oid FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.RepoConfig, 0)
	for rows.Next() {
		cfg, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRepo(s scanner) (model.RepoConfig, error) {
	var cfg model.RepoConfig
	var refresh int64
	var refreshNS int64
	var enabled int
	var preparedGitDir int
	if err := s.Scan(&cfg.ID, &cfg.Name, &cfg.MountRoot, &cfg.MountPath, &cfg.RemoteURL, &cfg.RemoteURLRedacted, &cfg.Branch, &refresh, &refreshNS, &cfg.GitDir, &cfg.OverlayDir, &cfg.BlobCacheDir, &cfg.MetaDBPath, &cfg.OverlayDBPath, &enabled, &preparedGitDir, &cfg.FetchRef, &cfg.PrepareState, &cfg.PrepareError, &cfg.Mode, &cfg.ExpectedOID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return cfg, fmt.Errorf("repo not found")
		}
		return cfg, err
	}
	if refreshNS > 0 {
		cfg.RefreshInterval = time.Duration(refreshNS)
	} else if refresh > 0 {
		cfg.RefreshInterval = time.Duration(refresh) * time.Second
	} else {
		cfg.RefreshInterval = 30 * time.Second
	}
	cfg.Enabled = enabled == 1
	cfg.PreparedGitDir = preparedGitDir == 1
	if cfg.Mode == "" {
		cfg.Mode = model.RepoModeWorkspace
	}
	return cfg, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func ensureRepoColumns(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(repos)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	add := map[string]string{
		"remote_url":          `TEXT NOT NULL DEFAULT ''`,
		"prepared_gitdir":     `INTEGER NOT NULL DEFAULT 0`,
		"fetch_ref":           `TEXT NOT NULL DEFAULT ''`,
		"prepare_state":       `TEXT NOT NULL DEFAULT ''`,
		"prepare_error":       `TEXT NOT NULL DEFAULT ''`,
		"refresh_interval_ns": `INTEGER NOT NULL DEFAULT 0`,
		"mode":                `TEXT NOT NULL DEFAULT 'workspace'`,
		"expected_oid":        `TEXT NOT NULL DEFAULT ''`,
	}
	for name, ddl := range add {
		if cols[name] {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE repos ADD COLUMN %s %s`, name, ddl)); err != nil {
			return err
		}
	}
	return nil
}
