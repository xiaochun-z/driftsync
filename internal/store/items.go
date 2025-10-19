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
	Sha1     string
	LastSrc  string
	LastSync int64
}

func UpsertItem(ctx context.Context, db *sql.DB, it Item) error {
	_, err := db.ExecContext(ctx, `INSERT INTO items(id, path_rel, etag, size, mtime, sha1, last_src, last_sync)
	VALUES (?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET path_rel=excluded.path_rel, etag=excluded.etag, size=excluded.size, mtime=excluded.mtime,
	sha1=excluded.sha1, last_src=excluded.last_src, last_sync=excluded.last_sync`,
		it.ID, it.PathRel, it.ETag, it.Size, it.Mtime, it.Sha1, it.LastSrc, it.LastSync)
	return err
}

func GetByPathFull(ctx context.Context, db *sql.DB, pathRel string) (*Item, error) {
	row := db.QueryRowContext(ctx, `SELECT id, path_rel, etag, size, mtime, 
		COALESCE(sha1, ''), COALESCE(last_src, ''), COALESCE(last_sync, 0) 
		FROM items WHERE path_rel = ?`, pathRel)
	var it Item
	if err := row.Scan(&it.ID, &it.PathRel, &it.ETag, &it.Size, &it.Mtime, &it.Sha1, &it.LastSrc, &it.LastSync); err != nil {
		return nil, err
	}
	return &it, nil
}

func ListAllPaths(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT path_rel FROM items`)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil { return nil, err }
		out = append(out, p)
	}
	return out, rows.Err()
}

func DeleteByPath(ctx context.Context, db *sql.DB, pathRel string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM items WHERE path_rel = ?`, pathRel)
	return err
}
