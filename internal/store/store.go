package store

import (
	"database/sql"
	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil { return nil, err }
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS tokens (
	id INTEGER PRIMARY KEY,
	access_token TEXT,
	refresh_token TEXT,
	expires_at INTEGER
);
CREATE TABLE IF NOT EXISTS items (
	id TEXT PRIMARY KEY,
	path_rel TEXT,
	etag TEXT,
	size INTEGER,
	mtime INTEGER,
	sha1 TEXT,
	last_src TEXT,
	last_sync INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_path ON items(path_rel);
`); err != nil { return nil, err }
	return db, nil
}
