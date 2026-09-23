package store

import (
	"context"
	"database/sql"
	"time"
)

// Job is one unit of metadata work: enrich this entity from this provider.
type Job struct {
	ID         int64
	EntityType string
	EntityID   int64
	Provider   string
	Attempts   int
}

const (
	JobPending = "pending"
	JobDone    = "done"
	JobFailed  = "failed"
)

// EnqueueJob records work to do. Re-enqueuing an entity that already succeeded
// is a no-op; one that failed is revived.
func (s *Store) EnqueueJob(ctx context.Context, entityType string, entityID int64, provider string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enrich_jobs (entity_type, entity_id, provider, state, next_run_at)
		 VALUES (?,?,?,'pending',0)
		 ON CONFLICT(entity_type, entity_id, provider) DO UPDATE SET
		     state = CASE WHEN enrich_jobs.state = 'failed' THEN 'pending' ELSE enrich_jobs.state END,
		     next_run_at = CASE WHEN enrich_jobs.state = 'failed' THEN 0 ELSE enrich_jobs.next_run_at END`,
		entityType, entityID, provider)
	return err
}

// ResetJobs makes finished work due again. Enqueuing alone never revives a job
// that already succeeded — otherwise every startup would re-hammer the providers
// — so a deliberate re-enrichment goes through here.
func (s *Store) ResetJobs(ctx context.Context, provider string) error {
	if provider == "" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE enrich_jobs SET state = 'pending', attempts = 0, next_run_at = 0`)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE enrich_jobs SET state = 'pending', attempts = 0, next_run_at = 0 WHERE provider = ?`, provider)
	return err
}

// DueJobs returns pending jobs whose backoff has elapsed, oldest first.
func (s *Store) DueJobs(ctx context.Context, now time.Time, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, entity_type, entity_id, provider, attempts FROM enrich_jobs
		 WHERE state = 'pending' AND next_run_at <= ? ORDER BY next_run_at, id LIMIT ?`,
		now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.EntityType, &j.EntityID, &j.Provider, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) FinishJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE enrich_jobs SET state = 'done', last_error = NULL WHERE id = ?`, id)
	return err
}

// RetryJob reschedules a failed attempt. After maxAttempts the job is parked as
// failed: a provider that never answers must not be retried forever. A
// maxAttempts of 0 parks it immediately, for failures that waiting cannot fix.
func (s *Store) RetryJob(ctx context.Context, id int64, next time.Time, reason string, maxAttempts int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE enrich_jobs SET
		     attempts = attempts + 1,
		     last_error = ?,
		     next_run_at = ?,
		     state = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END
		 WHERE id = ?`, reason, next.Unix(), maxAttempts, id)
	return err
}

type JobStats struct {
	Pending int `json:"pending"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
}

func (s *Store) JobStats(ctx context.Context) (JobStats, error) {
	var st JobStats
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM enrich_jobs GROUP BY state`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return st, err
		}
		switch state {
		case JobPending:
			st.Pending = n
		case JobDone:
			st.Done = n
		case JobFailed:
			st.Failed = n
		}
	}
	return st, rows.Err()
}

// ---- entities the enricher writes back ----

// AlbumForEnrich is what a provider needs to look an album up.
type AlbumForEnrich struct {
	ID     int64
	Title  string
	Artist string
	Year   int
	MBID   string
	Cover  string
}

func (s *Store) AlbumForEnrich(ctx context.Context, id int64) (AlbumForEnrich, error) {
	var a AlbumForEnrich
	var mbid, cover sql.NullString
	var year sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT al.id, al.title, ar.name, al.year, al.mbid, al.cover_path
		 FROM albums al JOIN artists ar ON ar.id = al.artist_id WHERE al.id = ?`, id).
		Scan(&a.ID, &a.Title, &a.Artist, &year, &mbid, &cover)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	a.Year, a.MBID, a.Cover = int(year.Int64), mbid.String, cover.String
	return a, err
}

// UpdateAlbumMeta writes provider results back. Empty values are left alone, so a
// second provider cannot erase what the first one found.
func (s *Store) UpdateAlbumMeta(ctx context.Context, id int64, mbid string, year int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE albums SET
		     mbid = COALESCE(NULLIF(?, ''), mbid),
		     year = COALESCE(NULLIF(?, 0), year)
		 WHERE id = ?`, mbid, year, id)
	return err
}

type ArtistForEnrich struct {
	ID   int64
	Name string
	MBID string
}

func (s *Store) ArtistForEnrich(ctx context.Context, id int64) (ArtistForEnrich, error) {
	var a ArtistForEnrich
	var mbid sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, name, mbid FROM artists WHERE id = ?`, id).
		Scan(&a.ID, &a.Name, &mbid)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	a.MBID = mbid.String
	return a, err
}

func (s *Store) UpdateArtistMeta(ctx context.Context, id int64, mbid string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE artists SET
		     mbid = COALESCE(NULLIF(?, ''), mbid),
		 WHERE id = ?`, mbid, id)
	return err
}

// AlbumIDsWithout lists albums with no MusicBrainz id, which is what the
// enrichment queue is seeded from.
func (s *Store) AlbumIDsWithout(ctx context.Context) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM albums WHERE mbid IS NULL OR mbid = ''`)
}

func (s *Store) ArtistIDsWithout(ctx context.Context) ([]int64, error) {
	return s.ids(ctx, `SELECT id FROM artists WHERE mbid IS NULL OR mbid = ''`)
}

func (s *Store) ids(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// ---- oauth tokens ----

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func (s *Store) SaveToken(ctx context.Context, provider string, t Token) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_tokens (provider, access_token, refresh_token, expires_at)
		 VALUES (?,?,?,?)
		 ON CONFLICT(provider) DO UPDATE SET access_token=excluded.access_token,
		   refresh_token=CASE WHEN excluded.refresh_token = '' THEN oauth_tokens.refresh_token
		                      ELSE excluded.refresh_token END,
		   expires_at=excluded.expires_at`,
		provider, t.AccessToken, t.RefreshToken, t.ExpiresAt.Unix())
	return err
}

func (s *Store) LoadToken(ctx context.Context, provider string) (Token, error) {
	var t Token
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT access_token, refresh_token, expires_at FROM oauth_tokens WHERE provider = ?`, provider).
		Scan(&t.AccessToken, &t.RefreshToken, &exp)
	if err == sql.ErrNoRows {
		return t, ErrNotFound
	}
	t.ExpiresAt = time.Unix(exp, 0)
	return t, err
}
