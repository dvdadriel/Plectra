package radio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const directoryAnswer = `[
  {"stationuuid":"a1","name":" Jazz FM ","url_resolved":"http://example/jazz.mp3",
   "codec":"MP3","bitrate":192,"country":"United Kingdom","tags":"jazz,smooth"},
  {"stationuuid":"a2","name":"AAC Only","url_resolved":"http://example/aac",
   "codec":"AAC","bitrate":64,"country":"France","tags":""},
  {"stationuuid":"a3","name":"No URL","url_resolved":"","codec":"MP3","bitrate":128},
  {"stationuuid":"a4","name":"Ogg Station","url_resolved":"http://example/ogg",
   "codec":"OGG","bitrate":128,"country":"Germany","tags":"classical"}
]`

func newDirectory(t *testing.T, h http.HandlerFunc) *Directory {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := New()
	d.BaseURL = srv.URL
	d.Client = srv.Client()
	return d
}

// A station Plectra cannot decode must not be listed: it would appear, be
// clicked, and then refuse to play, which is worse than not offering it.
func TestUndecodableAndUnplayableStationsAreLeftOut(t *testing.T) {
	d := newDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("no User-Agent sent; the directory asks clients to identify themselves")
		}
		w.Write([]byte(directoryAnswer))
	})

	got, _, err := d.Top(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d stations, want the MP3 and the Ogg only: %+v", len(got), got)
	}
	if got[0].Name != "Jazz FM" {
		t.Errorf("name = %q, want it trimmed", got[0].Name)
	}
	for _, s := range got {
		if !Playable(s.Codec) {
			t.Errorf("station %q has undecodable codec %q", s.Name, s.Codec)
		}
		if s.URL == "" {
			t.Errorf("station %q has no stream url", s.Name)
		}
	}
}

func TestSearchPassesTheQueryAndHidesBrokenStations(t *testing.T) {
	var got string
	d := newDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.Write([]byte(`[]`))
	})
	if _, _, err := d.Search(context.Background(), "jazz radio", 5, 0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name=jazz+radio", "hidebroken=true", "limit=5"} {
		if !contains(got, want) {
			t.Errorf("query %q is missing %q", got, want)
		}
	}
}

func TestDirectoryFailureIsReported(t *testing.T) {
	d := newDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	if _, _, err := d.Top(context.Background(), 5, 0); err == nil {
		t.Fatal("a failing directory reported success")
	}
}

func TestPlayableCodecs(t *testing.T) {
	for _, c := range []string{"MP3", "mp3", " Ogg ", "VORBIS"} {
		if !Playable(c) {
			t.Errorf("Playable(%q) = false", c)
		}
	}
	for _, c := range []string{"AAC", "AAC+", "FLAC", "", "UNKNOWN"} {
		if Playable(c) {
			t.Errorf("Playable(%q) = true, but Plectra cannot decode it", c)
		}
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func TestPagingSendsAnOffsetAndReportsWhetherMoreExist(t *testing.T) {
	var got string
	d := newDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.Write([]byte(directoryAnswer))
	})

	// directoryAnswer holds four stations, two of them playable.
	_, more, err := d.Top(context.Background(), 4, 40)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"offset=40", "limit=4"} {
		if !contains(got, want) {
			t.Errorf("query %q is missing %q", got, want)
		}
	}
	// Four rows came back for a page of four, so there is probably another —
	// even though filtering left only two. Deciding from the filtered count
	// would end the list early and hide stations that do play.
	if !more {
		t.Error("more = false on a full page; paging would stop at the first page with unplayable entries")
	}

	_, more, err = d.Top(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if more {
		t.Error("more = true on a page the directory did not fill")
	}
}
