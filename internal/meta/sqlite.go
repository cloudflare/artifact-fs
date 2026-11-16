package meta

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func OpenDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir db dir: %w", err)
	}
	params := url.Values{}
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "foreign_keys(ON)")
	// Every transaction takes the write lock at BEGIN, where it waits up to
	// busy_timeout for another connection's write. A deferred transaction that
	// reads and then writes fails its upgrade to a write at once with
	// SQLITE_BUSY, without the busy handler, while another connection holds
	// the write lock. A transaction begun with sql.TxOptions{ReadOnly: true}
	// stays deferred.
	params.Add("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ExecMigrations runs each statement on its own, outside a transaction. On a
// current schema each IF NOT EXISTS or IF EXISTS statement only reads, so an
// open takes no write lock and never waits for a writer, where a transaction
// would take the write lock at BEGIN. Each statement must be idempotent: an
// open that fails partway leaves the rest to the next open.
func ExecMigrations(ctx context.Context, db *sql.DB, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
