package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// newStore opens an empty database in a temp dir; every import test starts from
// a library and a history that are both empty.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// addTrack is shared by the tests in this package.
func addTrack(t *testing.T, st *store.Store, artist, title string) int64 {
	t.Helper()
	id, err := st.UpsertTrack(context.Background(), store.Track{
		Title: title, Artist: artist, Album: "Album", DiscNo: 1,
		Path: "/music/" + title + ".flac", FileHash: artist + "/" + title,
		DurationMS: 200000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRematchClaimsRowsAfterTrackEntersLibrary(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Rows imported by an earlier version: no local track matched them then.
	rows := []store.Play{
		{PlayedAt: 1700000000, MSPlayed: 200000, Completed: true, Source: "an-old-import",
			RawArtist: "Sigur Rós", RawTitle: "Hoppípolla"},
		{PlayedAt: 1700003600, MSPlayed: 210000, Completed: true, Source: "an-old-import",
			RawArtist: "Sigur Rós", RawTitle: "Hoppípolla (Live)"},
	}
	if n, err := st.ImportPlays(ctx, rows); err != nil || n != 2 {
		t.Fatalf("seeded %d rows, err = %v", n, err)
	}
	if stats, _ := st.HistoryStats(ctx); stats.Unmatched != 2 {
		t.Fatalf("unmatched = %d, want 2", stats.Unmatched)
	}

	rec := New(st)
	if claimed, err := rec.Rematch(ctx); err != nil || claimed != 0 {
		t.Fatalf("claimed %d with an empty library, err = %v", claimed, err)
	}

	// The file finally arrives, spelled differently, as real libraries are.
	id, err := st.UpsertTrack(ctx, store.Track{
		Title: "Hoppipolla", Artist: "Sigur Ros", Album: "Takk...", DiscNo: 1,
		Path: "/m/hoppipolla.flac", FileHash: "h1", DurationMS: 268000,
	})
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := rec.Rematch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != 2 {
		t.Fatalf("rematch claimed %d rows, want 2", claimed)
	}
	if n, _ := st.PlayCount(ctx, id); n != 2 {
		t.Fatalf("play count = %d, want 2", n)
	}
	if stats, _ := st.HistoryStats(ctx); stats.Unmatched != 0 {
		t.Fatalf("unmatched = %d after rematch, want 0", stats.Unmatched)
	}
}

// Podcast episodes and empty rows carry no track name, and an export arrives as
// a directory of numbered files, all of which must be read.
func at(min int) time.Time {
	return time.Date(2024, 5, 1, 12, min, 0, 0, time.UTC)
}
