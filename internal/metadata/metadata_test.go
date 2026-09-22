package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// fakeClock makes rate limits and backoff testable without waiting for them.
type fakeClock struct {
	now    time.Time
	slepts []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slepts = append(c.slepts, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func TestLimiterSpacesRequests(t *testing.T) {
	c := &fakeClock{now: time.Unix(1000, 0)}
	l := newLimiter(c, time.Second)
	ctx := context.Background()

	if err := l.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.slepts) != 0 {
		t.Fatalf("first request slept %v, want none", c.slepts)
	}
	if err := l.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.slepts) != 1 || c.slepts[0] != time.Second {
		t.Fatalf("second request slept %v, want one second", c.slepts)
	}

	// A caller that waited on its own must not be delayed again.
	c.now = c.now.Add(5 * time.Second)
	if err := l.wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.slepts) != 1 {
		t.Fatalf("slept again after a long gap: %v", c.slepts)
	}
}

func TestMusicBrainzParsesReleaseGroups(t *testing.T) {
	var gotQuery, gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAgent = r.URL.Query().Get("query"), r.Header.Get("User-Agent")
		w.Write([]byte(`{"release-groups":[
			{"id":"rg-1","title":"Mezzanine","score":98,"first-release-date":"1998-04-20",
			 "artist-credit":[{"name":"Massive Attack"}]}]}`))
	}))
	defer srv.Close()

	mb := NewMusicBrainz(&fakeClock{now: time.Unix(0, 0)})
	mb.BaseURL = srv.URL

	got, err := mb.Lookup(context.Background(), Query{Kind: KindAlbum, Artist: "Massive Attack", Album: "Mezzanine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "rg-1" || got[0].Year != 1998 || got[0].Artist != "Massive Attack" {
		t.Fatalf("match = %+v", got)
	}
	if got[0].CoverURL == "" {
		t.Fatal("no cover art url derived from the release-group id")
	}
	if gotAgent == "" {
		t.Fatal("no User-Agent sent; MusicBrainz refuses those")
	}
	if want := `releasegroup:"Mezzanine" AND artist:"Massive Attack"`; gotQuery != want {
		t.Fatalf("query = %q, want %q", gotQuery, want)
	}
}

func TestMusicBrainzEscapesUserInput(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	mb := NewMusicBrainz(&fakeClock{})
	mb.BaseURL = srv.URL
	// A title carrying Lucene syntax must not reach the search server intact.
	_, err := mb.Lookup(context.Background(), Query{Kind: KindArtist, Artist: `AC/DC" OR artist:*`})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"`, `:`, `^`, `~`} {
		if idx := indexAfterPrefix(gotQuery, bad); idx {
			t.Fatalf("unescaped %q survived into query %q", bad, gotQuery)
		}
	}
}

// indexAfterPrefix reports whether the operator appears outside the wrapping quotes.
func indexAfterPrefix(query, op string) bool {
	inner := query
	if len(inner) > len(`artist:""`) {
		inner = inner[len(`artist:"`) : len(inner)-1]
	}
	for i := 0; i+len(op) <= len(inner); i++ {
		if inner[i:i+len(op)] == op {
			return true
		}
	}
	return false
}

func TestRetryAfterIsHonouredAndJobsGiveUp(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	trackID, err := st.UpsertTrack(ctx, store.Track{
		Title: "T", Artist: "A", Album: "Alb", DiscNo: 1, Path: "/m/t.flac", FileHash: "h",
	})
	if err != nil || trackID == 0 {
		t.Fatal(err)
	}
	albums, _ := st.Albums(ctx)
	albumID := albums[0].ID

	clock := &fakeClock{now: time.Unix(10000, 0)}
	p := &stubProvider{err: &RetryableError{After: 90 * time.Second, Status: 503}}
	w := NewWorker(st, t.TempDir(), clock, p)

	if err := st.EnqueueJob(ctx, string(KindAlbum), albumID, p.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The provider said 90 seconds: the job must not be due before then.
	if jobs, _ := st.DueJobs(ctx, clock.now.Add(80*time.Second), 10); len(jobs) != 0 {
		t.Fatalf("job ran early: %+v", jobs)
	}
	jobs, _ := st.DueJobs(ctx, clock.now.Add(91*time.Second), 10)
	if len(jobs) != 1 || jobs[0].Attempts != 1 {
		t.Fatalf("after the wait, due jobs = %+v", jobs)
	}

	// Keep failing: the job eventually parks itself instead of retrying forever.
	for i := 0; i < maxAttempts+1; i++ {
		clock.now = clock.now.Add(24 * time.Hour)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.Pending != 0 {
		t.Fatalf("stats = %+v, want one failed job", stats)
	}
}

func TestWorkerWritesMatchesAndIgnoresWeakOnes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.UpsertTrack(ctx, store.Track{
		Title: "T", Artist: "A", Album: "Alb", DiscNo: 1, Path: "/m/t.flac", FileHash: "h",
	}); err != nil {
		t.Fatal(err)
	}
	albums, _ := st.Albums(ctx)
	id := albums[0].ID
	clock := &fakeClock{now: time.Unix(10000, 0)}

	// A low-confidence match is discarded: a confident wrong answer is worse.
	weak := &stubProvider{matches: []Match{{ID: "wrong", Score: 40, Year: 1999}}}
	w := NewWorker(st, t.TempDir(), clock, weak)
	if err := st.EnqueueJob(ctx, string(KindAlbum), id, weak.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ := st.AlbumForEnrich(ctx, id)
	if a.MBID != "" {
		t.Fatalf("weak match was written: %+v", a)
	}

	// The job is done now, and enqueuing again is deliberately a no-op; a
	// re-enrichment has to be asked for.
	if err := st.EnqueueJob(ctx, string(KindAlbum), id, weak.Name()); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := st.DueJobs(ctx, clock.now, 10); len(jobs) != 0 {
		t.Fatalf("finished job was revived by a plain enqueue: %+v", jobs)
	}

	strong := &stubProvider{matches: []Match{{ID: "right", Score: 95, Year: 1998}}}
	w = NewWorker(st, t.TempDir(), clock, strong)
	if err := st.ResetJobs(ctx, strong.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	a, _ = st.AlbumForEnrich(ctx, id)
	if a.MBID != "right" || a.Year != 1998 {
		t.Fatalf("album after enrichment = %+v", a)
	}
	if stats, _ := st.JobStats(ctx); stats.Done == 0 {
		t.Fatalf("job not marked done: %+v", stats)
	}
}

type stubProvider struct {
	matches []Match
	err     error
	calls   int
}

func (s *stubProvider) Name() string { return "musicbrainz" }
func (s *stubProvider) Lookup(ctx context.Context, q Query) ([]Match, error) {
	s.calls++
	return s.matches, s.err
}

func TestAuthStateIsRandomAndSingleUse(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sp := NewSpotify(st, "id", "secret", "http://127.0.0.1:4533/api/spotify/callback", &fakeClock{})

	first := stateOf(t, sp.StartAuth())
	if first == "" {
		t.Fatal("no state in the authorize url")
	}
	second := stateOf(t, sp.StartAuth())
	if first == second {
		t.Fatal("the same state was issued twice; it has to be unguessable per login")
	}

	// A callback carrying someone else's state is refused.
	if sp.CheckState(first) {
		t.Fatal("a stale state was accepted")
	}
	if !sp.CheckState(second) {
		t.Fatal("the state this server issued was rejected")
	}
	// And it cannot be replayed.
	if sp.CheckState(second) {
		t.Fatal("the same state was accepted twice")
	}
	if sp.CheckState("") {
		t.Fatal("an empty state was accepted")
	}
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestLinkedReportsWhetherAnAccountIsConnected(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	sp := NewSpotify(st, "id", "secret", "http://127.0.0.1:4533/cb", &fakeClock{})
	if sp.Linked(ctx) {
		t.Fatal("reported linked with no token stored")
	}
	// An access token with no refresh token cannot outlive the hour: not linked.
	if err := st.SaveToken(ctx, "spotify", store.Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if sp.Linked(ctx) {
		t.Fatal("reported linked without a refresh token")
	}
	if err := st.SaveToken(ctx, "spotify", store.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if !sp.Linked(ctx) {
		t.Fatal("reported not linked with a full token stored")
	}
}
