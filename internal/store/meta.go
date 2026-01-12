package store

import (
	"context"
	"database/sql"
)

func GetMeta(ctx context.Context, db *sql.DB, key string) (string, error) {
	row := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key)
	var val string
	if err := row.Scan(&val); err != nil {
		return "", err
	}
	return val, nil
}

func SetMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?) 
	ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
