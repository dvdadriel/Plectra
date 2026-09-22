package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

type stubProvider struct {
	byArtist map[string][]string
	calls    int
	err      error
}

func (s *stubProvider) Name() string { return "stub" }
func (s *stubProvider) SimilarArtists(_ context.Context, name, mbid string, limit int) ([]string, error) {
	s.calls++
	return s.byArtist[name], s.err
}

func newFixture(t *testing.T) (*store.Store, context.Context, map[string]int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	ids := map[string]int64{}
	add := func(title, artist string) {
		id, err := st.UpsertTrack(ctx, store.Track{
			Title: title, Artist: artist, Album: artist + " album", DiscNo: 1,
			Path: "/m/" + artist + "-" + title + ".flac", FileHash: artist + title,
			DurationMS: 200000,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[title] = id
	}
	add("Loved", "Massive Attack")
	add("Stale", "Portishead") // played long ago
	add("Fresh", "Tricky")     // never played
	add("Neighbour", "Bjork")  // only reachable through similarity
	return st, ctx, ids
}

func play(t *testing.T, st *store.Store, ctx context.Context, id int64, at time.Time) {
	t.Helper()
	if err := st.AddPlay(ctx, store.Play{
		TrackID: id, PlayedAt: at.Unix(), MSPlayed: 200000, Completed: true, Source: "plectra",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSectionsRankLocalTracksFromLocalListening(t *testing.T) {
	st, ctx, ids := newFixture(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	play(t, st, ctx, ids["Loved"], now.AddDate(0, 0, -2))
	play(t, st, ctx, ids["Loved"], now.AddDate(0, 0, -1))
	play(t, st, ctx, ids["Stale"], now.AddDate(0, -8, 0)) // eight months ago

	sim := &stubProvider{byArtist: map[string][]string{
		// Portishead is already a favourite: suggesting it back is not discovery.
		"Massive Attack": {"Portishead", "Bjork", "Someone Not In The Library"},
		"Portishead":     {"Massive Attack", "Bjork"},
	}}
	svc := New(st, sim)
	svc.Now = func() time.Time { return now }

	sections, err := svc.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string][]string{}
	for _, s := range sections {
		for _, tr := range s.Tracks {
			byTitle[s.Title] = append(byTitle[s.Title], tr.Title)
		}
	}

	// Similarity is resolved back to the library: Bjork is in, the artist we do
	// not own is dropped rather than offered and unplayable, and the artists
	// already being played are not recommended back.
	if got := byTitle["Because you play Massive Attack"]; len(got) != 1 || got[0] != "Neighbour" {
		t.Fatalf("similar section = %v, want just the local Bjork track", got)
	}
	if got := byTitle["Not heard in a while"]; len(got) != 1 || got[0] != "Stale" {
		t.Fatalf("stale section = %v, want Stale", got)
	}
	// Loved was played two days ago, so it is neither stale nor unplayed.
	for _, title := range byTitle["Never played"] {
		if title == "Loved" || title == "Stale" {
			t.Fatalf("never-played section contains %q", title)
		}
	}
	if len(byTitle["Never played"]) == 0 {
		t.Fatal("never-played section is empty; Fresh and Neighbour qualify")
	}
}

func TestWithoutListeningThereIsNothingToRecommendFromTaste(t *testing.T) {
	st, ctx, _ := newFixture(t)
	sim := &stubProvider{byArtist: map[string][]string{"Massive Attack": {"Bjork"}}}
	svc := New(st, sim)

	sections, err := svc.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sections {
		if s.Title != "Never played" {
			t.Fatalf("with no plays recorded, got section %q; only never-played can be honest", s.Title)
		}
	}
	if sim.calls != 0 {
		t.Fatalf("asked the provider %d times with no seeds", sim.calls)
	}
}

// A dead provider degrades the view; it never fails the request.
func TestProviderFailureLeavesTheLocalSections(t *testing.T) {
	st, ctx, ids := newFixture(t)
	now := time.Now()
	play(t, st, ctx, ids["Loved"], now)

	svc := New(st, &stubProvider{err: context.DeadlineExceeded})
	svc.Now = func() time.Time { return now }

	sections, err := svc.Sections(ctx)
	if err != nil {
		t.Fatalf("a failing provider failed the whole request: %v", err)
	}
	if len(sections) == 0 {
		t.Fatal("no sections at all; never-played should still be there")
	}
}

func TestNoProvidersStillWorks(t *testing.T) {
	st, ctx, ids := newFixture(t)
	play(t, st, ctx, ids["Loved"], time.Now())

	if _, err := New(st).Sections(ctx); err != nil {
		t.Fatalf("discovery without any provider failed: %v", err)
	}
}
