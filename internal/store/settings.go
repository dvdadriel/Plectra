package store

import (
	"context"
	"database/sql"
)

// Setting keys. What survives a restart lives here: the Spotify grant, and the
// name of the account it belongs to.
//
// No client id or secret among them. Plectra uses PKCE, which needs no secret,
// and the client id comes from the command line or the environment — a value
// the user types once is not worth a settings table.
const (
	KeySpotifyRefreshToken = "spotify_refresh_token"
	KeySpotifyAccount      = "spotify_account"
)

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Setting returns the stored value, or empty when unset. A missing setting is
// not an error: everything here is optional.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}
