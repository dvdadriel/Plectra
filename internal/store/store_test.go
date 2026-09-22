package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, context.Background()
}

func addTrack(t *testing.T, st *Store, ctx context.Context, title, artist, album string) int64 {
	t.Helper()
	id, err := st.UpsertTrack(ctx, Track{
		Title: title, Artist: artist, Album: album, DiscNo: 1,
		Path: "/music/" + title + ".flac", FileHash: title + artist,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSearchFindsTracksAlbumsAndArtists(t *testing.T) {
	st, ctx := newTestStore(t)
	addTrack(t, st, ctx, "Bohemian Rhapsody", "Queen", "A Night at the Opera")
	addTrack(t, st, ctx, "Under Pressure", "Queen", "Hot Space")
	addTrack(t, st, ctx, "Teardrop", "Massive Attack", "Mezzanine")

	res, err := st.Search(ctx, "bohem", 20) // prefix match, wrong case
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 1 || res.Tracks[0].Title != "Bohemian Rhapsody" {
		t.Fatalf("tracks = %+v", res.Tracks)
	}
	if len(res.Albums) != 1 || res.Albums[0].Title != "A Night at the Opera" {
		t.Fatalf("albums = %+v", res.Albums)
	}

	res, err = st.Search(ctx, "queen", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 2 || len(res.Artists) != 1 {
		t.Fatalf("artist search: %d tracks, %d artists", len(res.Tracks), len(res.Artists))
	}

	// Empty and operator-looking input must not error.
	for _, q := range []string{"", "   ", `AND "`, "*"} {
		if _, err := st.Search(ctx, q, 20); err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
	}
}

func TestSearchIndexFollowsTrackUpdates(t *testing.T) {
	st, ctx := newTestStore(t)
	addTrack(t, st, ctx, "Old Title", "Artist", "Album")

	// Same file_hash, new title: the FTS row must follow.
	if _, err := st.UpsertTrack(ctx, Track{
		Title: "New Title", Artist: "Artist", Album: "Album", DiscNo: 1,
		Path: "/music/Old Title.flac", FileHash: "Old TitleArtist",
	}); err != nil {
		t.Fatal(err)
	}
	if res, _ := st.Search(ctx, "Old", 20); len(res.Tracks) != 0 {
		t.Fatalf("stale title still indexed: %+v", res.Tracks)
	}
	if res, _ := st.Search(ctx, "New", 20); len(res.Tracks) != 1 {
		t.Fatalf("new title not indexed")
	}
}

func TestPlaylistLifecycle(t *testing.T) {
	st, ctx := newTestStore(t)
	a := addTrack(t, st, ctx, "A", "Artist", "Album")
	b := addTrack(t, st, ctx, "B", "Artist", "Album")
	c := addTrack(t, st, ctx, "C", "Artist", "Album")

	pl, err := st.CreatePlaylist(ctx, "Road trip", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddToPlaylist(ctx, pl.ID, []int64{a, b, c}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddToPlaylist(ctx, pl.ID, []int64{a}); err != nil {
		t.Fatal(err) // adding a duplicate is a no-op, not an error
	}
	if err := st.ReorderPlaylist(ctx, pl.ID, []int64{c, a, b}); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveFromPlaylist(ctx, pl.ID, a); err != nil {
		t.Fatal(err)
	}

	got, err := st.PlaylistTracks(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != c || got[1].ID != b {
		t.Fatalf("order after reorder+remove = %v", ids(got))
	}

	lists, err := st.Playlists(ctx)
	if err != nil || len(lists) != 1 || lists[0].TrackCount != 2 {
		t.Fatalf("playlists = %+v, err = %v", lists, err)
	}
	if err := st.DeletePlaylist(ctx, pl.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePlaylist(ctx, pl.ID); err != ErrNotFound {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

func TestLikesAndPlaysAreEvents(t *testing.T) {
	st, ctx := newTestStore(t)
	a := addTrack(t, st, ctx, "A", "Artist", "Album")

	if err := st.Like(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.Like(ctx, a); err != nil {
		t.Fatal(err) // liking twice stays one like
	}
	liked, _ := st.LikedTracks(ctx)
	if len(liked) != 1 {
		t.Fatalf("liked = %v", ids(liked))
	}
	if err := st.Unlike(ctx, a); err != nil {
		t.Fatal(err)
	}
	if liked, _ := st.LikedTracks(ctx); len(liked) != 0 {
		t.Fatalf("unlike left %v", ids(liked))
	}

	for i := 0; i < 3; i++ {
		if err := st.AddPlay(ctx, Play{TrackID: a, PlayedAt: int64(1000 + i), MSPlayed: 200000, Completed: true, Source: "plectra"}); err != nil {
			t.Fatal(err)
		}
	}
	// A skip is recorded too, but does not count as a play.
	if err := st.AddPlay(ctx, Play{TrackID: a, PlayedAt: 2000, MSPlayed: 500, Source: "plectra"}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PlayCount(ctx, a); err != nil || n != 3 {
		t.Fatalf("PlayCount = %d, err = %v, want 3", n, err)
	}
}

func TestPlayerStateRoundTrips(t *testing.T) {
	st, ctx := newTestStore(t)
	if _, err := st.LoadPlayerState(ctx); err != ErrNotFound {
		t.Fatalf("empty load = %v, want ErrNotFound", err)
	}
	want := PlayerState{TrackIDs: []int64{3, 1, 2}, Index: 1, PositionMS: 4200, Volume: 0.4, Shuffle: true, Repeat: "all"}
	if err := st.SavePlayerState(ctx, want); err != nil {
		t.Fatal(err)
	}
	want.PositionMS = 9000
	if err := st.SavePlayerState(ctx, want); err != nil {
		t.Fatal(err) // saving again updates the single row
	}
	got, err := st.LoadPlayerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Index != 1 || got.PositionMS != 9000 || got.Volume != 0.4 || !got.Shuffle ||
		got.Repeat != "all" || len(got.TrackIDs) != 3 || got.TrackIDs[0] != 3 {
		t.Fatalf("round trip = %+v", got)
	}
}

func ids(ts []Track) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
