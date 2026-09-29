package history

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

// newStore opens an empty database in a temp dir; every import test starts from
// a library and a history that are both empty.
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
