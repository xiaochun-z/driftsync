package store

import (
	"context"
	"database/sql"
)

type Item struct {
	ID       string
	PathRel  string
	ETag     string
	Size     int64
	Mtime    int64
	Shasum   string
	LastSrc  string
	LastSync int64
}

func UpsertItem(ctx context.Context, db *sql.DB, it Item) error {
	// 1. Pre-cleanup: Ensure no OTHER item occupies this path (fixes UNIQUE constraint violation)
	// If an item exists with the same path_rel but a DIFFERENT ID, delete it first.
	_, _ = db.ExecContext(ctx, `DELETE FROM items WHERE path_rel = ? AND id != ?`, it.PathRel, it.ID)

	// 2. Perform the Upsert
	_, err := db.ExecContext(ctx, `INSERT INTO items(id, path_rel, etag, size, mtime, shasum, last_src, last_sync)
	VALUES (?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET path_rel=excluded.path_rel, etag=excluded.etag, size=excluded.size, mtime=excluded.mtime,
	shasum=excluded.shasum, last_src=excluded.last_src, last_sync=excluded.last_sync`,
		it.ID, it.PathRel, it.ETag,  it.Size, it.Mtime, it.Shasum, it.LastSrc, it.LastSync)
	return err
}

func GetByPathFull(ctx context.Context, db *sql.DB, pathRel string) (*Item, error) {
	row := db.QueryRowContext(ctx, `SELECT id, path_rel, etag, size, mtime, 
		COALESCE(shasum, ''), COALESCE(last_src, ''), COALESCE(last_sync, 0) 
		FROM items WHERE path_rel = ?`, pathRel)
	var it Item
	if err := row.Scan(&it.ID, &it.PathRel, &it.ETag, &it.Size, &it.Mtime, &it.Shasum, &it.LastSrc, &it.LastSync); err != nil {
		return nil, err
	}
	return &it, nil
}

func ListAllPaths(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT path_rel FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func DeleteByPath(ctx context.Context, db *sql.DB, pathRel string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM items WHERE path_rel = ?`, pathRel)
	return err
}
