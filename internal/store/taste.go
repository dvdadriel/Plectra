package store

import "context"

// ArtistAffinity is how much one artist has actually been listened to, as
// opposed to how much of them sits in the library.
type ArtistAffinity struct {
	ArtistID   int64  `json:"artistId"`
	Name       string `json:"name"`
	MBID       string `json:"mbid,omitempty"`
	Plays      int    `json:"plays"`
	LastPlayed int64  `json:"lastPlayed,omitempty"`
}

// TopArtists ranks artists by completed plays, newest listening first when the
// counts tie. These are the seeds recommendations grow from.
func (s *Store) TopArtists(ctx context.Context, limit int) ([]ArtistAffinity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ar.id, ar.name, COALESCE(ar.mbid, ''), COUNT(*) AS plays, MAX(p.played_at)
		 FROM plays p
		 JOIN tracks t  ON t.id = p.track_id
		 JOIN artists ar ON ar.id = t.artist_id
		 WHERE p.completed = 1
		 GROUP BY ar.id
		 ORDER BY plays DESC, MAX(p.played_at) DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArtistAffinity{}
	for rows.Next() {
		var a ArtistAffinity
		if err := rows.Scan(&a.ArtistID, &a.Name, &a.MBID, &a.Plays, &a.LastPlayed); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TracksNeverPlayed lists library tracks with no completed play. Owning music
// you have never heard is the most common reason to want a recommendation.
func (s *Store) TracksNeverPlayed(ctx context.Context, limit int) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+
		` WHERE NOT EXISTS (
		      SELECT 1 FROM plays p WHERE p.track_id = t.id AND p.completed = 1
		  )
		  ORDER BY RANDOM() LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

// TracksNotHeardSince lists tracks played before but not since the given time,
// least recently heard first.
func (s *Store) TracksNotHeardSince(ctx context.Context, before int64, limit int) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, trackSelect+
		` JOIN (
		      SELECT track_id, MAX(played_at) AS last
		      FROM plays WHERE completed = 1 AND track_id IS NOT NULL
		      GROUP BY track_id
		      HAVING last < ?
		  ) recent ON recent.track_id = t.id
		  ORDER BY recent.last ASC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	return s.scanTracks(rows)
}

// TracksByArtistNames finds local tracks by artist name, which is how an
// external "similar artists" answer is turned back into something playable.
func (s *Store) TracksByArtistNames(ctx context.Context, names []string, perArtist int) ([]Track, error) {
	out := []Track{}
	for _, name := range names {
		rows, err := s.db.QueryContext(ctx, trackSelect+
			` WHERE ar.name = ? COLLATE NOCASE ORDER BY RANDOM() LIMIT ?`, name, perArtist)
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

// ArtistMBIDs returns the MusicBrainz ids of artists in the library, keyed by
// name, for matching external answers that speak in ids.
func (s *Store) ArtistMBIDs(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, COALESCE(mbid, '') FROM artists WHERE mbid IS NOT NULL AND mbid <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, mbid string
		if err := rows.Scan(&name, &mbid); err != nil {
			return nil, err
		}
		out[name] = mbid
	}
	return out, rows.Err()
}
