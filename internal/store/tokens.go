package store

import (
	"context"
	"database/sql"
	"time"
)

type TokenStore struct{ db *sql.DB }

func NewTokenStore(db *sql.DB) *TokenStore { return &TokenStore{db: db} }

type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func (s *TokenStore) Load(ctx context.Context) (*Tokens, error) {
	row := s.db.QueryRowContext(ctx, `SELECT access_token, refresh_token, expires_at FROM tokens WHERE id=1`)
	var a, r string
	var exp int64
	if err := row.Scan(&a, &r, &exp); err != nil {
		return nil, err
	}
	return &Tokens{AccessToken: a, RefreshToken: r, ExpiresAt: time.Unix(exp,0)}, nil
}

func (s *TokenStore) Save(ctx context.Context, t *Tokens) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens(id, access_token, refresh_token, expires_at)
	VALUES (1,?,?,?)
	ON CONFLICT(id) DO UPDATE SET access_token=excluded.access_token, refresh_token=excluded.refresh_token, expires_at=excluded.expires_at`,
		t.AccessToken, t.RefreshToken, t.ExpiresAt.Unix())
	return err
}
