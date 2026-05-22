package store

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// Open creates or opens the SQLite database at path and ensures the schema exists.
func Open(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS tokens (
			id INTEGER PRIMARY KEY,
			access_token TEXT,
			refresh_token TEXT,
			expires_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS items (
			id TEXT PRIMARY KEY,
			path_rel TEXT,
			etag TEXT,
			size INTEGER,
			mtime INTEGER,
			shasum TEXT,
			last_src TEXT,
			last_sync INTEGER
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_items_path ON items(path_rel)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT
		)`,
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// --- Meta (key-value store for sync state such as the delta link) ---

func GetMeta(ctx context.Context, db *sql.DB, key string) (string, error) {
	var val string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&val)
	return val, err
}

func SetMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// --- Token storage ---

// Tokens holds an OAuth2 access/refresh token pair.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// TokenStore persists OAuth2 tokens in the database.
type TokenStore struct{ db *sql.DB }

func NewTokenStore(db *sql.DB) *TokenStore { return &TokenStore{db: db} }

func (s *TokenStore) Load(ctx context.Context) (*Tokens, error) {
	var a, r string
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT access_token, refresh_token, expires_at FROM tokens WHERE id = 1`,
	).Scan(&a, &r, &exp)
	if err != nil {
		return nil, err
	}
	return &Tokens{AccessToken: a, RefreshToken: r, ExpiresAt: time.Unix(exp, 0)}, nil
}

func (s *TokenStore) Save(ctx context.Context, t *Tokens) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(id, access_token, refresh_token, expires_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   access_token=excluded.access_token,
		   refresh_token=excluded.refresh_token,
		   expires_at=excluded.expires_at`,
		t.AccessToken, t.RefreshToken, t.ExpiresAt.Unix())
	return err
}
