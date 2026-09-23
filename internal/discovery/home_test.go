package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/chart"
)

type stubCharts struct {
	trending, fresh []chart.Album
	err             error
}

func (s *stubCharts) Trending(context.Context, int) ([]chart.Album, error) {
	return s.trending, s.err
}
func (s *stubCharts) New(context.Context, int) ([]chart.Album, error) { return s.fresh, s.err }

func titles(secs []AlbumSection) []string {
	out := make([]string, 0, len(secs))
	for _, s := range secs {
		out = append(out, s.Title)
	}
	return out
}

func find(secs []AlbumSection, title string) *AlbumSection {
	for i := range secs {
		if secs[i].Title == title {
			return &secs[i]
		}
	}
	return nil
}

func TestHomeLeavesOutSectionsWithNothingInThem(t *testing.T) {
	st, ctx, _ := newFixture(t)
	svc := New(st) // nothing played, no charts wired

	secs, err := svc.Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"Pick up where you left off", "Played the most",
		"New releases", "Trending this week"} {
		if find(secs, unwanted) != nil {
			t.Errorf("%q appeared with no listening and no charts: %v", unwanted, titles(secs))
		}
	}
	// The library itself is there, so those two sections must be.
	if find(secs, "Your library") == nil || find(secs, "Recently added") == nil {
		t.Errorf("sections = %v, want the library ones", titles(secs))
	}
}

func TestHomeRanksAlbumsByListening(t *testing.T) {
	st, ctx, ids := newFixture(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Portishead played three times long ago; Tricky once, yesterday.
	play(t, st, ctx, ids["Stale"], now.AddDate(0, -8, 0))
	play(t, st, ctx, ids["Stale"], now.AddDate(0, -7, 0))
	play(t, st, ctx, ids["Stale"], now.AddDate(0, -6, 0))
	play(t, st, ctx, ids["Fresh"], now.AddDate(0, 0, -1))

	svc := New(st)
	svc.Now = func() time.Time { return now }
	secs, err := svc.Home(ctx)
	if err != nil {
		t.Fatal(err)
	}

	recent := find(secs, "Pick up where you left off")
	if recent == nil || len(recent.Albums) == 0 {
		t.Fatalf("no recent section: %v", titles(secs))
	}
	if recent.Albums[0].Artist != "Tricky" {
		t.Errorf("most recent album is by %q, want Tricky (played yesterday)", recent.Albums[0].Artist)
	}

	top := find(secs, "Played the most")
	if top == nil || len(top.Albums) == 0 {
		t.Fatalf("no most-played section: %v", titles(secs))
	}
	if top.Albums[0].Artist != "Portishead" {
		t.Errorf("most played is by %q, want Portishead (three plays)", top.Albums[0].Artist)
	}

	// Library albums must carry an id and no MBID: the view routes on that.
	for _, a := range top.Albums {
		if a.LocalID == 0 || a.MBID != "" {
			t.Errorf("library album %+v is not identified as local", a)
		}
	}
}

func TestHomeIncludesChartsAndSurvivesThemFailing(t *testing.T) {
	st, ctx, _ := newFixture(t)

	svc := New(st).WithCharts(&stubCharts{
		trending: []chart.Album{{MBID: "mb-1", Title: "Proof", Artist: "BTS"}},
		fresh:    []chart.Album{{MBID: "mb-2", Title: "Big", Artist: "Loud Band"}},
	})
	secs, err := svc.Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := find(secs, "Trending this week"); s == nil || s.Albums[0].Title != "Proof" {
		t.Errorf("trending section missing or wrong: %v", titles(secs))
	}
	if s := find(secs, "New releases"); s == nil || s.Albums[0].Title != "Big" {
		t.Errorf("new releases section missing or wrong: %v", titles(secs))
	}

	// Someone else's server going down costs a section, never the page.
	down := New(st).WithCharts(&stubCharts{err: errors.New("listenbrainz is down")})
	secs, err = down.Home(ctx)
	if err != nil {
		t.Fatalf("a failing chart broke the whole home view: %v", err)
	}
	if find(secs, "Your library") == nil {
		t.Errorf("local sections vanished with the charts: %v", titles(secs))
	}
	if find(secs, "Trending this week") != nil {
		t.Errorf("a failed chart still produced a section: %v", titles(secs))
	}
}

// Every section has to justify itself; a reason-less row is a slot machine.
func TestEverySectionSaysWhyItIsThere(t *testing.T) {
	st, ctx, ids := newFixture(t)
	play(t, st, ctx, ids["Loved"], time.Now())

	secs, err := New(st).Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) == 0 {
		t.Fatal("no sections at all")
	}
	for _, s := range secs {
		if s.Title == "" || s.Reason == "" {
			t.Errorf("section %+v is missing a title or a reason", s)
		}
		if len(s.Albums) == 0 {
			t.Errorf("section %q is empty and should have been left out", s.Title)
		}
	}
}

// "Because of what you play" must not appear before anything has been played.
// Sections() also returns a never-played band, which exists for a silent
// library too, so the row would otherwise carry a heading that is untrue.
func TestNoTasteRowBeforeAnythingIsPlayed(t *testing.T) {
	st, ctx, ids := newFixture(t)

	secs, err := New(st).Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if find(secs, "Because of what you play") != nil {
		t.Errorf("a taste row appeared with no listening at all: %v", titles(secs))
	}

	// One completed play is enough for the row to be honest.
	play(t, st, ctx, ids["Loved"], time.Now())
	secs, err = New(st).Home(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if find(secs, "Because of what you play") == nil {
		t.Errorf("no taste row after a completed play: %v", titles(secs))
	}
}
