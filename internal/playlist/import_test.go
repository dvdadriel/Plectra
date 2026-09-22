package playlist

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

func newService(t *testing.T) (*Service, *store.Store, context.Context) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), st, context.Background()
}

func writeExport(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportKeepsOnlyTracksTheLibraryHas(t *testing.T) {
	svc, st, ctx := newService(t)

	// Spelled differently on disk than on Spotify, as real libraries are.
	local, err := st.UpsertTrack(ctx, store.Track{
		Title: "Hoppipolla", Artist: "Sigur Ros", Album: "Takk...", DiscNo: 1,
		Path: "/m/hoppipolla.flac", FileHash: "h1",
	})
	if err != nil {
		t.Fatal(err)
	}

	path := writeExport(t, `{
      "source": "spotify-web-page",
      "playlists": [
        {"name": "Quiet", "spotifyId": "p1", "tracks": [
          {"name": "Hoppípolla", "artists": ["Sigur Rós"], "album": "Takk..."},
          {"name": "Not Owned", "artists": ["Someone"], "album": "X"}
        ]},
        {"name": "Nothing Local", "spotifyId": "p2", "tracks": [
          {"name": "Also Missing", "artists": ["Nobody"], "album": "Y"}
        ]}
      ],
      "liked": [{"name": "Hoppípolla", "artists": ["Sigur Rós"], "album": "Takk..."}]
    }`)

	res, err := svc.ImportFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Playlists != 1 || res.Matched != 1 || res.Liked != 1 {
		t.Fatalf("result = %+v, want one playlist, one match, one like", res)
	}
	if len(res.Unmatched) != 2 {
		t.Fatalf("unmatched = %v, want both missing tracks reported", res.Unmatched)
	}
	// A playlist with nothing local is named, not created empty.
	if len(res.Empty) != 1 || res.Empty[0] != "Nothing Local" {
		t.Fatalf("empty = %v", res.Empty)
	}

	lists, _ := st.Playlists(ctx)
	if len(lists) != 1 || lists[0].Name != "Quiet" {
		t.Fatalf("playlists = %+v", lists)
	}
	tracks, _ := st.PlaylistTracks(ctx, lists[0].ID)
	if len(tracks) != 1 || tracks[0].ID != local {
		t.Fatalf("playlist contents = %v", tracks)
	}
	liked, _ := st.LikedTracks(ctx)
	if len(liked) != 1 {
		t.Fatalf("liked = %v", liked)
	}
}

// A track credited to several artists should match when the library files it
// under any one of them.
func TestImportMatchesAnyCreditedArtist(t *testing.T) {
	svc, st, ctx := newService(t)
	if _, err := st.UpsertTrack(ctx, store.Track{
		Title: "Under Pressure", Artist: "David Bowie", Album: "Hot Space", DiscNo: 1,
		Path: "/m/up.flac", FileHash: "h1",
	}); err != nil {
		t.Fatal(err)
	}

	path := writeExport(t, `{"playlists":[{"name":"Duets","tracks":[
	   {"name":"Under Pressure","artists":["Queen","David Bowie"],"album":"Hot Space"}]}]}`)

	res, err := svc.ImportFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 {
		t.Fatalf("matched = %d, want the track found under its second artist", res.Matched)
	}
}

func TestImportRejectsSomethingThatIsNotAnExport(t *testing.T) {
	svc, _, ctx := newService(t)
	for _, body := range []string{`not json at all`, `{"playlists":[]}`} {
		if _, err := svc.ImportFile(ctx, writeExport(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	if _, err := svc.ImportFile(ctx, "/no/such/file.json"); err == nil {
		t.Error("accepted a missing file")
	}
}
