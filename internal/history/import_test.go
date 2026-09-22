package history

import (
	"context"
	"os"
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

// writeExport drops a GDPR export file into dir and returns its path. Fixtures
// are generated here rather than committed: the shape is the thing under test.
func writeExport(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// addTrack puts one file in the library. title doubles as the file hash, which
// is the track identity as far as the store is concerned.
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

// The current export shape: two listens, only one of which is in the library.
// The stranger is kept with its raw metadata — dropping it would make the
// statistics lie.
const currentExport = `[
  {"ts":"2024-01-05T20:11:03Z","ms_played":183000,
   "master_metadata_track_name":"Svefn-g-englar",
   "master_metadata_album_artist_name":"Sigur Rós",
   "master_metadata_album_album_name":"Ágætis byrjun",
   "spotify_track_uri":"spotify:track:0cQbJU3aWfsFbhGCWjJcnl"},
  {"ts":"2024-01-05T20:21:44Z","ms_played":240000,
   "master_metadata_track_name":"Untitled 3",
   "master_metadata_album_artist_name":"Sigur Rós",
   "master_metadata_album_album_name":"( )",
   "spotify_track_uri":"spotify:track:1AbCdEfGhIjKlMnOpQrStU"}
]`

func TestImportGDPRCurrentFormatKeepsUnmatchedRows(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Only the first of the two listens exists locally, and it is spelled
	// without its accents — the match key has to bridge that.
	local := addTrack(t, st, "sigur ros", "Svefn-g-englar")

	dir := t.TempDir()
	writeExport(t, dir, "StreamingHistory_music_0.json", currentExport)

	res, err := New(st).ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 || res.Imported != 2 || res.Matched != 1 || res.Unmatched != 1 {
		t.Fatalf("result = %+v, want rows 2, imported 2, matched 1, unmatched 1", res)
	}

	// Read it back through the store rather than trusting the return value.
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 2 || stats.Unmatched != 1 {
		t.Fatalf("stats = %+v, want total 2, unmatched 1", stats)
	}
	if got := stats.BySource[store.SourceSpotifyExport]; got != 2 {
		t.Fatalf("bySource[%s] = %d, want 2", store.SourceSpotifyExport, got)
	}

	// The matched row is attached to the local track...
	if n, err := st.PlayCount(ctx, local); err != nil || n != 1 {
		t.Fatalf("play count for matched track = %d (err %v), want 1", n, err)
	}
	// ...and the unmatched one is still there, with the metadata it arrived with.
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 {
		t.Fatalf("unmatched = %+v, want exactly one row", un)
	}
	if un[0].Artist != "Sigur Rós" || un[0].Title != "Untitled 3" {
		t.Fatalf("unmatched row = %+v, want raw artist/title preserved", un[0])
	}
}

// The older export used endTime/msPlayed/trackName/artistName. Exports that
// arrived years ago must still import through the same path.
const olderExport = `[
  {"endTime":"2019-04-02 18:30","msPlayed":201000,
   "trackName":"Teardrop","artistName":"Massive Attack"},
  {"endTime":"2019-04-02 18:35","msPlayed":175000,
   "trackName":"Angel","artistName":"Massive Attack"}
]`

func TestImportGDPRAcceptsOlderExportFormat(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	local := addTrack(t, st, "Massive Attack", "Teardrop")

	file := writeExport(t, t.TempDir(), "StreamingHistory0.json", olderExport)

	res, err := New(st).ImportGDPR(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 || res.Imported != 2 || res.Matched != 1 || res.Unmatched != 1 {
		t.Fatalf("result = %+v, want rows 2, imported 2, matched 1, unmatched 1", res)
	}
	if n, err := st.PlayCount(ctx, local); err != nil || n != 1 {
		t.Fatalf("play count for Teardrop = %d (err %v), want 1", n, err)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0].Title != "Angel" || un[0].Artist != "Massive Attack" {
		t.Fatalf("unmatched = %+v, want the single Angel row", un)
	}
}

// The same export applied twice must not double every statistic. This is what
// the partial unique index on plays is for.
func TestImportGDPRTwiceAddsNothing(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	addTrack(t, st, "sigur ros", "Svefn-g-englar")

	dir := t.TempDir()
	writeExport(t, dir, "StreamingHistory_music_0.json", currentExport)
	rec := New(st)

	first, err := rec.ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	second, err := rec.ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 {
		t.Fatalf("second import added %d rows, want 0", second.Imported)
	}
	// The rows are still read and counted; they are simply not written again.
	if second.Rows != first.Rows {
		t.Fatalf("second import read %d rows, first read %d", second.Rows, first.Rows)
	}
	after, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != before.Total {
		t.Fatalf("total grew from %d to %d on re-import", before.Total, after.Total)
	}
}

// A history imported before the files arrived is worth keeping only if the rows
// are claimed once the track is scanned.
func TestRematchClaimsRowsAfterTrackEntersLibrary(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rec := New(st)

	dir := t.TempDir()
	writeExport(t, dir, "StreamingHistory_music_0.json", `[
      {"ts":"2024-02-01T10:00:00Z","ms_played":210000,
       "master_metadata_track_name":"Weightless",
       "master_metadata_album_artist_name":"Marconi Union",
       "master_metadata_album_album_name":"Ambient 1"},
      {"ts":"2024-02-01T10:10:00Z","ms_played":205000,
       "master_metadata_track_name":"Weightless",
       "master_metadata_album_artist_name":"Marconi Union",
       "master_metadata_album_album_name":"Ambient 1"}
    ]`)

	res, err := rec.ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unmatched != 2 || res.Matched != 0 {
		t.Fatalf("result = %+v, want both rows unmatched before the scan", res)
	}

	// The file finally shows up in the library.
	id := addTrack(t, st, "Marconi Union", "Weightless")

	claimed, err := rec.Rematch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed != 2 {
		t.Fatalf("rematch claimed %d rows, want 2", claimed)
	}
	if n, err := st.PlayCount(ctx, id); err != nil || n != 2 {
		t.Fatalf("play count after rematch = %d (err %v), want 2", n, err)
	}
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Unmatched != 0 || stats.Total != 2 {
		t.Fatalf("stats after rematch = %+v, want total 2, unmatched 0", stats)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 0 {
		t.Fatalf("unmatched rows left after rematch: %+v", un)
	}
}

// Podcast episodes and empty rows carry no track name, and an export arrives as
// a directory of numbered files, all of which must be read.
func TestImportSkipsTitlelessRowsAcrossAllFilesInDirectory(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	writeExport(t, dir, "StreamingHistory_music_0.json", `[
      {"ts":"2024-03-01T09:00:00Z","ms_played":190000,
       "master_metadata_track_name":"One",
       "master_metadata_album_artist_name":"Band"},
      {"ts":"2024-03-01T09:05:00Z","ms_played":900000,
       "master_metadata_track_name":null,
       "master_metadata_album_artist_name":null,
       "episode_name":"A Podcast Episode"}
    ]`)
	writeExport(t, dir, "StreamingHistory_music_1.json", `[
      {"ts":"2024-03-02T09:00:00Z","ms_played":195000,
       "master_metadata_track_name":"Two",
       "master_metadata_album_artist_name":"Band"}
    ]`)
	writeExport(t, dir, "StreamingHistory_music_2.json", `[
      {"ts":"2024-03-03T09:00:00Z","ms_played":198000,
       "master_metadata_track_name":"Three",
       "master_metadata_album_artist_name":"Band"}
    ]`)
	// Not a history file: the export ships other JSON next to it.
	writeExport(t, dir, "Userdata.json", `[{"ts":"2024-03-04T09:00:00Z",
       "ms_played":100000,"master_metadata_track_name":"Ignored",
       "master_metadata_album_artist_name":"Band"}]`)

	res, err := New(st).ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 {
		t.Fatalf("files = %d, want the 3 history files only", res.Files)
	}
	if res.Rows != 3 || res.Imported != 3 {
		t.Fatalf("result = %+v, want 3 rows imported (podcast row skipped)", res)
	}
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 3 {
		t.Fatalf("stored total = %d, want 3", stats.Total)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, u := range un {
		titles[u.Title] = true
	}
	if len(un) != 3 || !titles["One"] || !titles["Two"] || !titles["Three"] {
		t.Fatalf("stored rows = %+v, want One, Two and Three from all three files", un)
	}
}

// ---- the recently-played bridge ----

// listens builds the shape metadata.Spotify.RecentlyPlayed hands to the bridge.
// Timestamps are fixed: nothing here may depend on when the test runs.
func at(min int) time.Time {
	return time.Date(2024, 5, 1, 12, min, 0, 0, time.UTC)
}

func TestImportExternalMatchesLibraryAndKeepsStrangers(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	local := addTrack(t, st, "Portishead", "Roads")

	res, err := New(st).ImportExternal(ctx, store.SourceSpotifyAPI, []ExternalPlay{
		{PlayedAt: at(0), MSPlayed: 302000, Artist: "Portishead", Album: "Dummy",
			Title: "Roads", SpotifyID: "1nZzLMnJvOFNPMRfnc4bWD"},
		{PlayedAt: at(5), MSPlayed: 211000, Artist: "Boards of Canada",
			Album: "Geogaddi", Title: "Roygbiv", SpotifyID: "4L1kDsYb0cNM2F5PsR9OBf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 || res.Imported != 2 || res.Matched != 1 || res.Unmatched != 1 {
		t.Fatalf("result = %+v, want rows 2, imported 2, matched 1, unmatched 1", res)
	}

	// Rows land under the API source, not the export source.
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BySource[store.SourceSpotifyAPI] != 2 || stats.Total != 2 {
		t.Fatalf("stats = %+v, want 2 rows all under %s", stats, store.SourceSpotifyAPI)
	}
	// PlayCount only counts completed rows, so this proves both the track_id
	// and completed = true on the matched listen.
	if n, err := st.PlayCount(ctx, local); err != nil || n != 1 {
		t.Fatalf("completed plays for Roads = %d (err %v), want 1", n, err)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0].Title != "Roygbiv" || un[0].Artist != "Boards of Canada" {
		t.Fatalf("unmatched = %+v, want the single Roygbiv row with its raw metadata", un)
	}
	// The poller resumes from the newest row of this source.
	last, err := st.LastPlayedAt(ctx, store.SourceSpotifyAPI)
	if err != nil {
		t.Fatal(err)
	}
	if last != at(5).Unix() {
		t.Fatalf("LastPlayedAt = %d, want the newest listen %d", last, at(5).Unix())
	}
}

// The poller asks for a window that overlaps what it already stored. The
// overlap must add nothing, and the one genuinely new listen must land.
func TestImportExternalOverlappingWindowAddsOnlyTheNewListen(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rec := New(st)

	first := []ExternalPlay{
		{PlayedAt: at(0), MSPlayed: 302000, Artist: "Portishead", Title: "Roads"},
		{PlayedAt: at(5), MSPlayed: 211000, Artist: "Boards of Canada", Title: "Roygbiv"},
	}
	if _, err := rec.ImportExternal(ctx, store.SourceSpotifyAPI, first); err != nil {
		t.Fatal(err)
	}

	// Same two listens again, plus one that happened since.
	second := append(append([]ExternalPlay{}, first...),
		ExternalPlay{PlayedAt: at(9), MSPlayed: 180000, Artist: "Portishead", Title: "Glory Box"})
	res, err := rec.ImportExternal(ctx, store.SourceSpotifyAPI, second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 3 {
		t.Fatalf("rows read = %d, want all 3 of the window", res.Rows)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d, want only the 1 new listen", res.Imported)
	}
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 3 {
		t.Fatalf("stored total = %d after overlapping re-import, want 3", stats.Total)
	}
	last, err := st.LastPlayedAt(ctx, store.SourceSpotifyAPI)
	if err != nil {
		t.Fatal(err)
	}
	if last != at(9).Unix() {
		t.Fatalf("LastPlayedAt = %d, want %d", last, at(9).Unix())
	}
}

// ---- damaged input ----

// A directory where one history file is not valid JSON. The good files still
// import, and the result says a file was lost instead of quietly reporting a
// smaller directory.
func TestImportGDPRReportsUnreadableFileAndImportsTheRest(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	writeExport(t, dir, "StreamingHistory_music_0.json", `[
      {"ts":"2024-04-01T09:00:00Z","ms_played":190000,
       "master_metadata_track_name":"Good One",
       "master_metadata_album_artist_name":"Band"}
    ]`)
	// Truncated mid-write: the shape a killed export actually leaves behind.
	writeExport(t, dir, "StreamingHistory_music_1.json",
		`[{"ts":"2024-04-02T09:00:00Z","ms_played":190000,"master_metadata_tr`)
	writeExport(t, dir, "StreamingHistory_music_2.json", `[
      {"ts":"2024-04-03T09:00:00Z","ms_played":198000,
       "master_metadata_track_name":"Good Two",
       "master_metadata_album_artist_name":"Band"}
    ]`)

	res, err := New(st).ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatalf("one broken file must not fail the whole import: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("result = %+v, want failed 1 so the broken file is visible", res)
	}
	if res.Files != 2 || res.Rows != 2 || res.Imported != 2 {
		t.Fatalf("result = %+v, want the 2 readable files fully imported", res)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, u := range un {
		titles[u.Title] = true
	}
	if len(un) != 2 || !titles["Good One"] || !titles["Good Two"] {
		t.Fatalf("stored rows = %+v, want Good One and Good Two", un)
	}
}

// A row whose timestamp cannot be read is dropped; the rest of the file still
// imports. Neither an unknown layout nor an empty timestamp aborts anything.
func TestImportGDPRSkipsRowsWithUnparseableTimestamps(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	writeExport(t, dir, "StreamingHistory_music_0.json", `[
      {"ts":"not a timestamp","ms_played":190000,
       "master_metadata_track_name":"Garbled",
       "master_metadata_album_artist_name":"Band"},
      {"ts":"","endTime":"","ms_played":190000,
       "master_metadata_track_name":"No Time At All",
       "master_metadata_album_artist_name":"Band"},
      {"ts":"01/05/2024 20:11","ms_played":190000,
       "master_metadata_track_name":"Wrong Layout",
       "master_metadata_album_artist_name":"Band"},
      {"ts":"2024-04-05T09:00:00Z","ms_played":190000,
       "master_metadata_track_name":"Readable",
       "master_metadata_album_artist_name":"Band"}
    ]`)

	res, err := New(st).ImportGDPR(ctx, dir)
	if err != nil {
		t.Fatalf("a bad timestamp must not abort the import: %v", err)
	}
	if res.Files != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want the file itself read fine", res)
	}
	if res.Rows != 1 || res.Imported != 1 {
		t.Fatalf("result = %+v, want only the 1 readable row", res)
	}
	un, err := st.UnmatchedPlays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(un) != 1 || un[0].Title != "Readable" {
		t.Fatalf("stored rows = %+v, want only Readable", un)
	}
}
