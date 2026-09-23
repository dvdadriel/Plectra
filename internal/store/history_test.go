package store

import "testing"

// LastPlayedAt is the poller's resume point: a wrong answer silently re-imports
// or skips history, so both the empty case and the per-source case matter.
func TestLastPlayedAtIsPerSourceAndZeroWhenEmpty(t *testing.T) {
	st, ctx := newTestStore(t)

	// No rows at all: zero, and no error. MAX() over nothing is SQL NULL, which
	// is the easy way to get a scan error here instead.
	// Two arbitrary source names: what is under test is that the answer is
	// per-source, not any particular importer.
	const (
		sourceA = "importer-a"
		sourceB = "importer-b"
	)

	last, err := st.LastPlayedAt(ctx, sourceA)
	if err != nil {
		t.Fatalf("LastPlayedAt on an empty history returned %v, want no error", err)
	}
	if last != 0 {
		t.Fatalf("LastPlayedAt on an empty history = %d, want 0", last)
	}

	n, err := st.ImportPlays(ctx, []Play{
		{PlayedAt: 1700000000, Source: sourceA, RawArtist: "A", RawTitle: "One"},
		{PlayedAt: 1700003600, Source: sourceA, RawArtist: "A", RawTitle: "Two"},
		{PlayedAt: 1700001800, Source: sourceA, RawArtist: "A", RawTitle: "Three"},
		// A far newer row from another source must not move the first one.
		{PlayedAt: 1800000000, Source: sourceB, RawArtist: "A", RawTitle: "Old Export"},
	})
	if err != nil || n != 4 {
		t.Fatalf("ImportPlays added %d rows (err %v), want 4", n, err)
	}

	// The newest of the source, not the last row written.
	last, err = st.LastPlayedAt(ctx, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	if last != 1700003600 {
		t.Fatalf("LastPlayedAt(%s) = %d, want 1700003600", sourceA, last)
	}
	if last, err = st.LastPlayedAt(ctx, sourceB); err != nil || last != 1800000000 {
		t.Fatalf("LastPlayedAt(%s) = %d (err %v), want 1800000000", sourceB, last, err)
	}
	// A source that has never been imported is still a clean zero.
	if last, err = st.LastPlayedAt(ctx, SourcePlectra); err != nil || last != 0 {
		t.Fatalf("LastPlayedAt(%s) = %d (err %v), want 0", SourcePlectra, last, err)
	}
}
