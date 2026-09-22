// Package store owns the SQLite database. It is the only package that writes SQL.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// ponytail: schema.sql is idempotent DDL, no migration table yet.
	// Add numbered migrations when a column has to change under an existing DB.
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	db.SetMaxOpenConns(1) // ponytail: single writer, no lock contention to reason about
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Track struct {
	ID         int64  `json:"id"`
	AlbumID    int64  `json:"albumId"`
	ArtistID   int64  `json:"artistId"`
	Title      string `json:"title"`
	TrackNo    int    `json:"trackNo"`
	DiscNo     int    `json:"discNo"`
	DurationMS int64  `json:"durationMs"`
	Format     string `json:"format"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
	Path       string `json:"-"`
	FileHash   string `json:"-"`
	MTime      int64  `json:"-"`
	Album      string `json:"album"`
	Artist     string `json:"artist"`
	Year       int    `json:"-"` // written to the album row, not the track row
}

type Album struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Year   int    `json:"year"`
}

type Artist struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// UpsertTrack writes a scanned file, creating its artist and album as needed.
// file_hash is the identity: a moved or renamed file keeps its row.
func (s *Store) UpsertTrack(ctx context.Context, t Track) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	artistID, err := upsertName(ctx, tx, t.Artist)
	if err != nil {
		return 0, err
	}
	var albumID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO albums (title, artist_id, year) VALUES (?, ?, ?)
		 ON CONFLICT(title, artist_id) DO UPDATE SET year = COALESCE(NULLIF(excluded.year,0), albums.year)
		 RETURNING id`, t.Album, artistID, t.Year).Scan(&albumID); err != nil {
		return 0, err
	}

	var id int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO tracks (album_id, artist_id, title, track_no, disc_no, duration_ms,
		                     format, sample_rate, channels, path, file_hash, mtime, added_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(file_hash) DO UPDATE SET
		     album_id=excluded.album_id, artist_id=excluded.artist_id, title=excluded.title,
		     track_no=excluded.track_no, disc_no=excluded.disc_no, duration_ms=excluded.duration_ms,
		     format=excluded.format, sample_rate=excluded.sample_rate, channels=excluded.channels,
		     path=excluded.path, mtime=excluded.mtime
		 RETURNING id`,
		albumID, artistID, t.Title, t.TrackNo, t.DiscNo, t.DurationMS,
		t.Format, t.SampleRate, t.Channels, t.Path, t.FileHash, t.MTime,
		time.Now().Unix()).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func upsertName(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO artists (name, sort_name) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET name = excluded.name
		 RETURNING id`, name, name).Scan(&id)
	return id, err
}

// KnownMTime reports the stored mtime for a path, so a rescan can skip untouched files.
func (s *Store) KnownMTime(ctx context.Context, path string) (int64, bool) {
	var m int64
	err := s.db.QueryRowContext(ctx, `SELECT mtime FROM tracks WHERE path = ?`, path).Scan(&m)
	return m, err == nil
}

func (s *Store) Albums(ctx context.Context) ([]Album, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT al.id, al.title, ar.name, COALESCE(al.year,0)
		 FROM albums al JOIN artists ar ON ar.id = al.artist_id
		 ORDER BY ar.name, al.year, al.title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Album{}
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Artists(ctx context.Context) ([]Artist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM artists ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artist{}
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const trackSelect = `SELECT t.id, t.album_id, t.artist_id, t.title, t.track_no, t.disc_no,
	t.duration_ms, t.format, t.sample_rate, t.channels, t.path, t.file_hash,
	al.title, ar.name
	FROM tracks t JOIN albums al ON al.id = t.album_id JOIN artists ar ON ar.id = t.artist_id`

func (s *Store) scanTracks(rows *sql.Rows) ([]Track, error) {
	defer rows.Close()
	out := []Track{}
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.AlbumID, &t.ArtistID, &t.Title, &t.TrackNo, &t.DiscNo,
			&t.DurationMS, &t.Format, &t.SampleRate, &t.Channels, &t.Path, &t.FileHash,
			&t.Album, &t.Artist); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TracksByAlbum(ctx context.Context, albumID int64) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+` WHERE t.album_id = ? ORDER BY t.disc_no, t.track_no`, albumID)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

func (s *Store) TracksByArtist(ctx context.Context, artistID int64) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+` WHERE t.artist_id = ? ORDER BY al.title, t.disc_no, t.track_no`, artistID)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

func (s *Store) TracksByIDs(ctx context.Context, ids []int64) ([]Track, error) {
	out := make([]Track, 0, len(ids))
	for _, id := range ids { // ponytail: queue sizes are human-scale; batch with an IN clause if that changes
		rows, err := s.db.QueryContext(ctx, trackSelect+` WHERE t.id = ?`, id)
		if err != nil {
			return nil, err
		}
		ts, err := s.scanTracks(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}
