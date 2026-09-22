package store

import (
	"context"
	"database/sql"
)

// Setting keys. Credentials live here so they can be entered in the UI instead
// of on the command line, where they would also be visible in the process list.
const (
	KeySpotifyClientID     = "spotify_client_id"
	KeySpotifyClientSecret = "spotify_client_secret"
	KeyLastFMAPIKey        = "lastfm_api_key"
	KeyListenBrainzToken   = "listenbrainz_token"
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
