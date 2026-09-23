package browse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const dzSearch = `{"data":[
 {"id":14879699,"title":"OK Computer","record_type":"album",
  "cover_medium":"https://cdn-images.dzcdn.net/x/250x250.jpg",
  "artist":{"name":"Radiohead"}},
 {"id":999,"title":"Creep","record_type":"single",
  "cover_medium":"","artist":{"name":"Radiohead"}}]}`

const dzAlbum = `{"title":"OK Computer","artist":{"name":"Radiohead"},"tracks":{"data":[
 {"title":"Airbag","duration":287,"artist":{"name":"Radiohead"}},
 {"title":"Paranoid Android","duration":387,"artist":{"name":""}}]}}`

func newDeezer(t *testing.T, h http.HandlerFunc) *Deezer {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := NewDeezer()
	d.BaseURL = srv.URL
	d.HTTP = srv.Client()
	return d
}

func TestDeezerSearchMarksItsOwnIDsAndKeepsArtwork(t *testing.T) {
	var query string
	d := newDeezer(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte(dzSearch))
	})

	albums, err := d.SearchAlbums(context.Background(), "  radiohead  ", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "q=radiohead") || !strings.Contains(query, "limit=5") {
		t.Errorf("query = %q", query)
	}
	if len(albums) != 2 {
		t.Fatalf("got %d albums, want 2", len(albums))
	}
	// The prefix is how one endpoint serves two catalogues without a second
	// parameter that could drift out of sync with the id.
	if albums[0].ID != "dz:14879699" {
		t.Errorf("id = %q, want the dz: prefix", albums[0].ID)
	}
	if !d.Handles(albums[0].ID) {
		t.Errorf("Handles(%q) = false", albums[0].ID)
	}
	if d.Handles("b1392450-e666-3926-a536-22c65f834433") {
		t.Error("claimed a MusicBrainz id")
	}
	if albums[0].Cover == "" {
		t.Error("artwork was dropped; carrying it inline is the point of this source")
	}
	if albums[1].Type != "Single" {
		t.Errorf("type = %q, want Single capitalised like MusicBrainz's", albums[1].Type)
	}
}

func TestDeezerTracksConvertSecondsAndFallBackToTheAlbumArtist(t *testing.T) {
	var path string
	d := newDeezer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(dzAlbum))
	})

	tracks, err := d.Tracks(context.Background(), "dz:14879699")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/album/14879699" {
		t.Errorf("asked for %q; the dz: prefix should not reach the URL", path)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	// Deezer counts in seconds and Plectra everywhere else in milliseconds.
	if tracks[0].DurationMS != 287000 {
		t.Errorf("duration = %d, want 287000ms", tracks[0].DurationMS)
	}
	if tracks[0].Position != 1 || tracks[1].Position != 2 {
		t.Errorf("positions = %d, %d; the nested listing carries none, so order is it",
			tracks[0].Position, tracks[1].Position)
	}
	// A track with no artist of its own belongs to the album's.
	if tracks[1].Artist != "Radiohead" {
		t.Errorf("artist = %q, want the album's", tracks[1].Artist)
	}
}

func TestDeezerReportsItsOwnErrorBody(t *testing.T) {
	d := newDeezer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":{"message":"Quota limit exceeded"}}`))
	})
	_, err := d.SearchAlbums(context.Background(), "x", 5)
	if err == nil || !strings.Contains(err.Error(), "Quota limit") {
		t.Errorf("err = %v, want Deezer's own wording", err)
	}
}
