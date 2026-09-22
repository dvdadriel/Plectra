package store

import "testing"

// LastPlayedAt is the poller's resume point: a wrong answer silently re-imports
// or skips history, so both the empty case and the per-source case matter.
func TestLastPlayedAtIsPerSourceAndZeroWhenEmpty(t *testing.T) {
	st, ctx := newTestStore(t)

	// No rows at all: zero, and no error. MAX() over nothing is SQL NULL, which
	// is the easy way to get a scan error here instead.
	last, err := st.LastPlayedAt(ctx, SourceSpotifyAPI)
	if err != nil {
		t.Fatalf("LastPlayedAt on an empty history returned %v, want no error", err)
	}
	if last != 0 {
		t.Fatalf("LastPlayedAt on an empty history = %d, want 0", last)
	}

	n, err := st.ImportPlays(ctx, []Play{
		{PlayedAt: 1700000000, Source: SourceSpotifyAPI, RawArtist: "A", RawTitle: "One"},
		{PlayedAt: 1700003600, Source: SourceSpotifyAPI, RawArtist: "A", RawTitle: "Two"},
		{PlayedAt: 1700001800, Source: SourceSpotifyAPI, RawArtist: "A", RawTitle: "Three"},
		// A far newer row from another source must not move the API resume point.
		{PlayedAt: 1800000000, Source: SourceSpotifyExport, RawArtist: "A", RawTitle: "Old Export"},
	})
	if err != nil || n != 4 {
		t.Fatalf("ImportPlays added %d rows (err %v), want 4", n, err)
	}

	// The newest of the source, not the last row written.
	last, err = st.LastPlayedAt(ctx, SourceSpotifyAPI)
	if err != nil {
		t.Fatal(err)
	}
	if last != 1700003600 {
		t.Fatalf("LastPlayedAt(%s) = %d, want 1700003600", SourceSpotifyAPI, last)
	}
	if last, err = st.LastPlayedAt(ctx, SourceSpotifyExport); err != nil || last != 1800000000 {
		t.Fatalf("LastPlayedAt(%s) = %d (err %v), want 1800000000", SourceSpotifyExport, last, err)
	}
	// A source that has never been imported is still a clean zero.
	if last, err = st.LastPlayedAt(ctx, SourcePlectra); err != nil || last != 0 {
		t.Fatalf("LastPlayedAt(%s) = %d (err %v), want 0", SourcePlectra, last, err)
	}
}
