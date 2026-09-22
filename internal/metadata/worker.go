package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/plectra/plectra/internal/store"
)

const (
	maxAttempts = 5
	batchSize   = 20
)

// Worker drains the enrich_jobs table. It is deliberately separate from the
// scanner: scanning is fast, local and offline, enrichment is slow, remote and
// allowed to fail for hours.
type Worker struct {
	st        *store.Store
	providers map[string]Provider
	clock     Clock
	coverDir  string
	client    *http.Client
	// Interval is how long to idle when there is nothing due.
	Interval time.Duration
}

func NewWorker(st *store.Store, coverDir string, clock Clock, providers ...Provider) *Worker {
	m := map[string]Provider{}
	for _, p := range providers {
		m[p.Name()] = p
	}
	return &Worker{
		st: st, providers: m, clock: clock, coverDir: coverDir,
		client:   &http.Client{Timeout: 30 * time.Second},
		Interval: 5 * time.Second,
	}
}

// Seed queues every album and artist still missing an id from a provider, and
// reports how many jobs it queued.
func (w *Worker) Seed(ctx context.Context) (int, error) {
	n := 0
	for name := range w.providers {
		albums, err := w.st.AlbumIDsWithout(ctx, name)
		if err != nil {
			return n, err
		}
		for _, id := range albums {
			if err := w.st.EnqueueJob(ctx, string(KindAlbum), id, name); err != nil {
				return n, err
			}
			n++
		}
		artists, err := w.st.ArtistIDsWithout(ctx, name)
		if err != nil {
			return n, err
		}
		for _, id := range artists {
			if err := w.st.EnqueueJob(ctx, string(KindArtist), id, name); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// Reset makes finished work due again, for a deliberate re-enrichment.
func (w *Worker) Reset(ctx context.Context, provider string) error {
	return w.st.ResetJobs(ctx, provider)
}

// Stats reports queue progress for the UI.
func (w *Worker) Stats(ctx context.Context) (store.JobStats, error) {
	return w.st.JobStats(ctx)
}

// Run works the queue until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("enrich: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		if n == 0 {
			if err := w.clock.Sleep(ctx, w.Interval); err != nil {
				return
			}
		}
	}
}

// RunOnce processes one batch of due jobs and reports how many it handled.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	jobs, err := w.st.DueJobs(ctx, w.clock.Now(), batchSize)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if err := w.do(ctx, j); err != nil {
			w.reschedule(ctx, j, err)
			continue
		}
		if err := w.st.FinishJob(ctx, j.ID); err != nil {
			return 0, err
		}
	}
	return len(jobs), nil
}

// reschedule applies exponential backoff, or the provider's own Retry-After.
func (w *Worker) reschedule(ctx context.Context, j store.Job, cause error) {
	delay := time.Duration(math.Pow(2, float64(j.Attempts))) * 30 * time.Second
	var retry *RetryableError
	if errors.As(cause, &retry) && retry.After > 0 {
		delay = retry.After
	}
	next := w.clock.Now().Add(delay)
	if err := w.st.RetryJob(ctx, j.ID, next, cause.Error(), maxAttempts); err != nil {
		log.Printf("enrich: reschedule: %v", err)
	}
}

func (w *Worker) do(ctx context.Context, j store.Job) error {
	p, ok := w.providers[j.Provider]
	if !ok {
		return fmt.Errorf("unknown provider %q", j.Provider)
	}
	switch Kind(j.EntityType) {
	case KindAlbum:
		return w.enrichAlbum(ctx, p, j.EntityID)
	case KindArtist:
		return w.enrichArtist(ctx, p, j.EntityID)
	}
	return fmt.Errorf("unknown entity type %q", j.EntityType)
}

func (w *Worker) enrichAlbum(ctx context.Context, p Provider, id int64) error {
	a, err := w.st.AlbumForEnrich(ctx, id)
	if err != nil {
		return err
	}
	matches, err := p.Lookup(ctx, Query{Kind: KindAlbum, Artist: a.Artist, Album: a.Title, Year: a.Year})
	if err != nil {
		return err
	}
	best, ok := pick(matches)
	if !ok {
		return nil // nothing found is an answer, not a failure worth retrying
	}
	if p.Name() == "spotify" {
		err = w.st.UpdateAlbumMeta(ctx, id, "", best.ID, best.Year)
	} else {
		err = w.st.UpdateAlbumMeta(ctx, id, best.ID, "", best.Year)
	}
	if err != nil {
		return err
	}
	if a.Cover == "" && best.CoverURL != "" {
		if path, err := w.cacheCover(ctx, best.CoverURL); err == nil {
			return w.st.SetAlbumCover(ctx, id, path)
		}
		// Missing artwork is not worth failing the job over: ids were still saved.
	}
	return nil
}

func (w *Worker) enrichArtist(ctx context.Context, p Provider, id int64) error {
	a, err := w.st.ArtistForEnrich(ctx, id)
	if err != nil {
		return err
	}
	matches, err := p.Lookup(ctx, Query{Kind: KindArtist, Artist: a.Name})
	if err != nil {
		return err
	}
	best, ok := pick(matches)
	if !ok {
		return nil
	}
	if p.Name() == "spotify" {
		return w.st.UpdateArtistMeta(ctx, id, "", best.ID)
	}
	return w.st.UpdateArtistMeta(ctx, id, best.ID, "")
}

// pick takes the highest-scoring match, and only if the provider is reasonably
// sure. A confident wrong answer is worse than no answer.
func pick(matches []Match) (Match, bool) {
	best, ok := Match{}, false
	for _, m := range matches {
		if m.Score > best.Score {
			best, ok = m, true
		}
	}
	if !ok || best.Score < 70 {
		return Match{}, false
	}
	return best, true
}

// cacheCover downloads artwork into the cover cache and returns its path.
func (w *Worker) cacheCover(ctx context.Context, url string) (string, error) {
	if w.coverDir == "" {
		return "", errors.New("no cover directory configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cover art: status %d", resp.StatusCode)
	}

	if err := os.MkdirAll(w.coverDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(w.coverDir, "cover-*")
	if err != nil {
		return "", err
	}
	defer f.Close()

	// 8MB is generous for front cover art and keeps a bad URL from filling the disk.
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 8<<20)); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	ext := ".jpg"
	if resp.Header.Get("Content-Type") == "image/png" {
		ext = ".png"
	}
	final := f.Name() + ext
	if err := os.Rename(f.Name(), final); err != nil {
		return "", err
	}
	return filepath.Clean(final), nil
}
