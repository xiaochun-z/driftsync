package store

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	// Enable WAL mode (better concurrency) and set a busy timeout (prevent "database is locked" errors)
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Execute schema creation in separate steps to ensure reliability
	queries := []string{
		`CREATE TABLE IF NOT EXISTS tokens (
			id INTEGER PRIMARY KEY,
			access_token TEXT,
			refresh_token TEXT,
			expires_at INTEGER
		);`,
		`CREATE TABLE IF NOT EXISTS items (
			id TEXT PRIMARY KEY,
			path_rel TEXT,
						etag TEXT,
			size INTEGER,
			mtime INTEGER,
			shasum TEXT,
			last_src TEXT,
			last_sync INTEGER
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_items_path ON items(path_rel);`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT
		);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return nil, err
		}
	}
	return db, nil
}
