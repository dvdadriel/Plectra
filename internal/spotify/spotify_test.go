package spotify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseIDAcceptsTheShapesPeoplePaste(t *testing.T) {
	const id = "37i9dQZF1DXcBWIGoYBM5M"
	for _, in := range []string{
		id,
		"https://open.spotify.com/playlist/" + id,
		"https://open.spotify.com/playlist/" + id + "?si=abc123def456",
		"spotify:playlist:" + id,
	} {
		got, err := ParseID(in)
		if err != nil || got != id {
			t.Errorf("ParseID(%q) = %q, %v", in, got, err)
		}
	}
	// A track link is a different thing, and saying so beats a confusing empty
	// playlist later.
	if _, err := ParseID("https://open.spotify.com/track/" + id); err == nil {
		t.Error("a track link was accepted as a playlist")
	}
	for _, bad := range []string{"", "   ", "not a link"} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) succeeded", bad)
		}
	}
}

const embedPage = `<html><script id="__NEXT_DATA__" type="application/json">
{"props":{"pageProps":{"state":{"data":{"entity":{
  "name":"Quiet Evening","type":"playlist",
  "coverArt":{"sources":[{"url":"https://img/cover.jpg"}]},
  "trackList":[
    {"uri":"spotify:track:aaa","title":"Hoppípolla","subtitle":"Sigur Rós","duration":268000},
    {"uri":"spotify:track:bbb","title":"Svefn-g-englar","subtitle":"Sigur Rós","duration":600000}
  ]}}}}}}
</script></html>`

func TestFetchReadsTheEmbeddedPlaylist(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("no User-Agent sent")
		}
		w.Write([]byte(embedPage))
	}))
	defer srv.Close()

	c := New()
	c.BaseURL, c.HTTP = srv.URL, srv.Client()

	pl, err := c.Fetch(context.Background(), "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M?si=x")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/embed/playlist/37i9dQZF1DXcBWIGoYBM5M" {
		t.Fatalf("requested %q", path)
	}
	if pl.Name != "Quiet Evening" || len(pl.Tracks) != 2 {
		t.Fatalf("playlist = %+v", pl)
	}
	if pl.Tracks[0].SpotifyID != "aaa" || pl.Tracks[0].Artist != "Sigur Rós" || pl.Tracks[0].DurationMS != 268000 {
		t.Fatalf("first track = %+v", pl.Tracks[0])
	}
	if pl.Truncated {
		t.Error("a two-track playlist was reported as truncated")
	}
}

// Spotify hands over 25 tracks at most. Saying so is the difference between a
// short playlist and a cut-off one.
func TestFetchFlagsTruncation(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<script id="__NEXT_DATA__">{"props":{"pageProps":{"state":{"data":{"entity":{"name":"Long","trackList":[`)
	for i := 0; i < EmbedLimit; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"uri":"spotify:track:x","title":"T","subtitle":"A","duration":1000}`)
	}
	b.WriteString(`]}}}}}}</script>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	c := New()
	c.BaseURL, c.HTTP = srv.URL, srv.Client()
	pl, err := c.Fetch(context.Background(), "37i9dQZF1DXcBWIGoYBM5M")
	if err != nil {
		t.Fatal(err)
	}
	if !pl.Truncated {
		t.Fatalf("a playlist at the limit (%d tracks) was not flagged as truncated", len(pl.Tracks))
	}
}

// A private playlist comes back as an empty page; that must be said plainly and
// not reported as a playlist with no songs.
func TestPrivatePlaylistIsNamedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<script id="__NEXT_DATA__">{"props":{"pageProps":{"state":{"data":{"entity":{}}}}}}</script>`))
	}))
	defer srv.Close()

	c := New()
	c.BaseURL, c.HTTP = srv.URL, srv.Client()
	_, err := c.Fetch(context.Background(), "37i9dQZF1DXcBWIGoYBM5M")
	if err == nil {
		t.Fatal("a private playlist was accepted")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v, want it to mention that the playlist is private", err)
	}
}

func TestChangedEmbedIsReportedClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>no embedded state any more</html>`))
	}))
	defer srv.Close()

	c := New()
	c.BaseURL, c.HTTP = srv.URL, srv.Client()
	_, err := c.Fetch(context.Background(), "37i9dQZF1DXcBWIGoYBM5M")
	if err == nil || !strings.Contains(err.Error(), "embed") {
		t.Fatalf("error = %v, want it to say the embed shape changed", err)
	}
}
