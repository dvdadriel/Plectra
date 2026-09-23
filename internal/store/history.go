package store

import (
	"context"
	"database/sql"
)

// Sources of a play row. Only Plectra records plays today; the column stays
// because rows imported by an earlier version still name theirs.
const SourcePlectra = "plectra"

// ImportPlays writes imported history in one transaction. Rows that duplicate an
// earlier import are skipped, and the count of genuinely new rows is returned.
// Rows that match no local track keep their raw metadata: dropping them would
// make the statistics lie.
func (s *Store) ImportPlays(ctx context.Context, plays []Play) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO plays (track_id, played_at, ms_played, completed, source,
		                    raw_artist, raw_album, raw_title)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	added := 0
	for _, p := range plays {
		var trackID any
		if p.TrackID > 0 {
			trackID = p.TrackID
		}
		res, err := stmt.ExecContext(ctx, trackID, p.PlayedAt, p.MSPlayed, p.Completed,
			p.Source, p.RawArtist, p.RawAlbum, p.RawTitle)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// UnmatchedPlay is an imported row that found no local track.
type UnmatchedPlay struct {
	ID     int64
	Artist string
	Title  string
}

// UnmatchedPlays lists imported rows still waiting for their file to show up.
func (s *Store) UnmatchedPlays(ctx context.Context) ([]UnmatchedPlay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(raw_artist,''), COALESCE(raw_title,'')
		 FROM plays WHERE track_id IS NULL AND raw_title IS NOT NULL AND raw_title <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnmatchedPlay{}
	for rows.Next() {
		var u UnmatchedPlay
		if err := rows.Scan(&u.ID, &u.Artist, &u.Title); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ClaimPlays attaches previously unmatched rows to a track that has since been
// scanned. This is what makes an import done before the files arrive worth keeping.
func (s *Store) ClaimPlays(ctx context.Context, trackID int64, playIDs []int64) error {
	if len(playIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range playIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE plays SET track_id = ? WHERE id = ?`, trackID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastPlayedAt reports the newest play from a source, which is where a polling
// bridge resumes from.
func (s *Store) LastPlayedAt(ctx context.Context, source string) (int64, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(played_at) FROM plays WHERE source = ?`, source).Scan(&at)
	return at.Int64, err
}

// HistoryStats is what the UI shows about listening history.
type HistoryStats struct {
	Total     int            `json:"total"`
	Unmatched int            `json:"unmatched"`
	BySource  map[string]int `json:"bySource"`
}

func (s *Store) HistoryStats(ctx context.Context) (HistoryStats, error) {
	st := HistoryStats{BySource: map[string]int{}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, COUNT(*), SUM(CASE WHEN track_id IS NULL THEN 1 ELSE 0 END)
		 FROM plays GROUP BY source`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		var total, unmatched int
		if err := rows.Scan(&source, &total, &unmatched); err != nil {
			return st, err
		}
		st.BySource[source] = total
		st.Total += total
		st.Unmatched += unmatched
	}
	return st, rows.Err()
}
