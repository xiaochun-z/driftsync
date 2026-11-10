package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// BindSyncRoot prevents a different local directory from inheriting deletion
// history. Legacy databases without a binding are conservatively re-baselined.
// Authentication tokens are deliberately preserved.
func BindSyncRoot(ctx context.Context, db *sql.DB, root string) (bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return false, err
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='sync_root'`).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if old == abs {
		return false, tx.Commit()
	}
	var tracked int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&tracked); err != nil {
		return false, err
	}
	for _, q := range []string{
		`DELETE FROM items`,
		`DELETE FROM remote_nodes`,
		`DELETE FROM meta WHERE key='delta_link'`,
		`DELETE FROM meta WHERE key='pending_renames'`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return false, err
		}
	}
	if tracked > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('needs_cloud_baseline','1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('sync_root',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, abs); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
