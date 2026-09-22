package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a row a caller named does not exist.
var ErrNotFound = errors.New("not found")

// SearchResult is what one FTS5 query answers: every entity at once.
type SearchResult struct {
	Artists []Artist `json:"artists"`
	Albums  []Album  `json:"albums"`
	Tracks  []Track  `json:"tracks"`
}

// Search runs one FTS5 query over tracks and derives the matching albums and
// artists from it, so a typo in a track title still surfaces its album.
func (s *Store) Search(ctx context.Context, q string, limit int) (SearchResult, error) {
	out := SearchResult{Artists: []Artist{}, Albums: []Album{}, Tracks: []Track{}}
	q = strings.TrimSpace(q)
	if q == "" {
		return out, nil
	}
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, trackSelect+
		` JOIN tracks_fts f ON f.rowid = t.id WHERE tracks_fts MATCH ? ORDER BY rank LIMIT ?`,
		ftsQuery(q), limit)
	if err != nil {
		return out, err
	}
	out.Tracks, err = s.scanTracks(rows)
	if err != nil {
		return out, err
	}

	seenAlbum, seenArtist := map[int64]bool{}, map[int64]bool{}
	for _, t := range out.Tracks {
		if !seenAlbum[t.AlbumID] {
			seenAlbum[t.AlbumID] = true
			out.Albums = append(out.Albums, Album{ID: t.AlbumID, Title: t.Album, Artist: t.Artist})
		}
		if !seenArtist[t.ArtistID] {
			seenArtist[t.ArtistID] = true
			out.Artists = append(out.Artists, Artist{ID: t.ArtistID, Name: t.Artist})
		}
	}
	return out, nil
}

// ftsQuery turns user input into a prefix query and keeps FTS5 operators from
// being interpreted — a quote or a stray AND must not make the search error out.
func ftsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		w = strings.ReplaceAll(w, `"`, "")
		if w == "" {
			continue
		}
		terms = append(terms, `"`+w+`"*`)
	}
	if len(terms) == 0 {
		return `""`
	}
	return strings.Join(terms, " ")
}

// ---- playlists ----

type Playlist struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	TrackCount  int    `json:"trackCount"`
}

func (s *Store) CreatePlaylist(ctx context.Context, name, desc string) (Playlist, error) {
	now := time.Now().Unix()
	p := Playlist{Name: name, Description: desc, CreatedAt: now, UpdatedAt: now}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO playlists (name, description, created_at, updated_at)
		 VALUES (?,?,?,?) RETURNING id`, name, desc, now, now).Scan(&p.ID)
	return p, err
}

func (s *Store) Playlists(ctx context.Context) ([]Playlist, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.name, p.description, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id)
		 FROM playlists p ORDER BY p.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Playlist{}
	for rows.Next() {
		var p Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt, &p.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdatePlaylist(ctx context.Context, id int64, name, desc string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE playlists SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		name, desc, time.Now().Unix(), id)
	return checkAffected(res, err)
}

func (s *Store) DeletePlaylist(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM playlists WHERE id = ?`, id)
	return checkAffected(res, err)
}

// AddToPlaylist appends tracks at the end, skipping ones already in the list.
func (s *Store) AddToPlaylist(ctx context.Context, id int64, trackIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), -1) + 1 FROM playlist_items WHERE playlist_id = ?`, id).Scan(&next); err != nil {
		return err
	}
	for _, tid := range trackIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, track_id, position) VALUES (?,?,?)
			 ON CONFLICT DO NOTHING`, id, tid, next); err != nil {
			return err
		}
		next++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RemoveFromPlaylist(ctx context.Context, id, trackID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ? AND track_id = ?`, id, trackID)
	return err
}

// ReorderPlaylist rewrites positions to match the given track order.
func (s *Store) ReorderPlaylist(ctx context.Context, id int64, trackIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	for pos, tid := range trackIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO playlist_items (playlist_id, track_id, position) VALUES (?,?,?)`, id, tid, pos); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playlists SET updated_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PlaylistTracks(ctx context.Context, id int64) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+
		` JOIN playlist_items i ON i.track_id = t.id WHERE i.playlist_id = ? ORDER BY i.position`, id)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

// ---- likes ----

func (s *Store) Like(ctx context.Context, trackID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO likes (track_id, liked_at) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		trackID, time.Now().Unix())
	return err
}

func (s *Store) Unlike(ctx context.Context, trackID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM likes WHERE track_id = ?`, trackID)
	return err
}

func (s *Store) LikedTracks(ctx context.Context) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+
		` JOIN likes l ON l.track_id = t.id ORDER BY l.liked_at DESC`)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

func (s *Store) LikedIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT track_id FROM likes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- plays ----

type Play struct {
	TrackID   int64
	PlayedAt  int64
	MSPlayed  int64
	Completed bool
	Source    string
	RawArtist string
	RawAlbum  string
	RawTitle  string
}

func (s *Store) AddPlay(ctx context.Context, p Play) error {
	var trackID any
	if p.TrackID > 0 {
		trackID = p.TrackID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO plays (track_id, played_at, ms_played, completed, source, raw_artist, raw_album, raw_title)
		 VALUES (?,?,?,?,?,?,?,?)`,
		trackID, p.PlayedAt, p.MSPlayed, p.Completed, p.Source, p.RawArtist, p.RawAlbum, p.RawTitle)
	return err
}

func (s *Store) PlayCount(ctx context.Context, trackID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plays WHERE track_id = ? AND completed = 1`, trackID).Scan(&n)
	return n, err
}

// RecentPlays lists the most recent plays with their track, newest first.
func (s *Store) RecentPlays(ctx context.Context, limit int) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+
		` JOIN plays p ON p.track_id = t.id GROUP BY t.id ORDER BY MAX(p.played_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

// ---- player state ----

// PlayerState persists what has to survive a restart. The queue is stored as
// track ids, not rows: the track table is the truth, and a renamed file must not
// resurrect a stale path.
type PlayerState struct {
	TrackIDs   []int64 `json:"trackIds"`
	Index      int     `json:"index"`
	PositionMS int64   `json:"positionMs"`
	Volume     float64 `json:"volume"`
	Shuffle    bool    `json:"shuffle"`
	Repeat     string  `json:"repeat"`
}

func (s *Store) SavePlayerState(ctx context.Context, st PlayerState) error {
	b, err := json.Marshal(st.TrackIDs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO player_state (id, queue_json, idx, position_ms, volume, shuffle, repeat)
		 VALUES (1,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET queue_json=excluded.queue_json, idx=excluded.idx,
		   position_ms=excluded.position_ms, volume=excluded.volume,
		   shuffle=excluded.shuffle, repeat=excluded.repeat`,
		string(b), st.Index, st.PositionMS, st.Volume, st.Shuffle, st.Repeat)
	return err
}

func (s *Store) LoadPlayerState(ctx context.Context) (PlayerState, error) {
	var st PlayerState
	var qj string
	err := s.db.QueryRowContext(ctx,
		`SELECT queue_json, idx, position_ms, volume, shuffle, repeat FROM player_state WHERE id = 1`).
		Scan(&qj, &st.Index, &st.PositionMS, &st.Volume, &st.Shuffle, &st.Repeat)
	if err == sql.ErrNoRows {
		return PlayerState{Volume: 1, Repeat: "off", Index: -1}, ErrNotFound
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal([]byte(qj), &st.TrackIDs)
	return st, err
}

func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- cover art ----

// AlbumCoverState reports the album a track belongs to and whether that album
// already has artwork cached.
func (s *Store) AlbumCoverState(ctx context.Context, trackID int64) (int64, bool, error) {
	var albumID int64
	var cover sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT al.id, al.cover_path FROM tracks t JOIN albums al ON al.id = t.album_id WHERE t.id = ?`,
		trackID).Scan(&albumID, &cover)
	if err != nil {
		return 0, false, err
	}
	return albumID, cover.Valid && cover.String != "", nil
}

func (s *Store) SetAlbumCover(ctx context.Context, albumID int64, path string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE albums SET cover_path = ? WHERE id = ?`, path, albumID)
	return err
}

// AlbumCover returns the cached artwork path for an album.
func (s *Store) AlbumCover(ctx context.Context, albumID int64) (string, error) {
	var cover sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT cover_path FROM albums WHERE id = ?`, albumID).Scan(&cover); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", err
	}
	if !cover.Valid || cover.String == "" {
		return "", ErrNotFound
	}
	return cover.String, nil
}

// AllTracks is used to build match indexes for imports.
func (s *Store) AllTracks(ctx context.Context) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}
