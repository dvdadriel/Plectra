package chart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Recorded from the real ListenBrainz on 2026-09-23, trimmed to three entries.
const trendingBody = `{"payload":{"count":3,"range":"week","release_groups":[
 {"artist_name":"BTS","caa_id":44649533593,"caa_release_mbid":"bd01f665-425a-44d3-8926-e550417236f5",
  "listen_count":291991,"release_group_mbid":"aaa-1","release_group_name":"Proof"},
 {"artist_name":"Radiohead","caa_id":43287476628,"caa_release_mbid":"30702389-5c67-4438-9ea0-2351c8de0f1d",
  "listen_count":120000,"release_group_mbid":"aaa-2","release_group_name":"OK Computer"},
 {"artist_name":"No Art","caa_id":0,"caa_release_mbid":"",
  "listen_count":10,"release_group_mbid":"aaa-3","release_group_name":"Uncatalogued"}]}}`

// fresh-releases as it really answers: mostly noise with no artwork and no
// listeners, which is the whole reason this is filtered before it is shown.
const freshBody = `{"payload":{"releases":[
 {"artist_credit_name":"Nobody","caa_id":null,"caa_release_mbid":null,"listen_count":0,
  "release_date":"2026-09-09","release_group_mbid":"f-1","release_name":"Unheard","release_group_primary_type":"Album"},
 {"artist_credit_name":"Quiet Band","caa_id":111,"caa_release_mbid":"rel-1","listen_count":5,
  "release_date":"2026-09-10","release_group_mbid":"f-2","release_name":"Small","release_group_primary_type":"Album"},
 {"artist_credit_name":"Loud Band","caa_id":222,"caa_release_mbid":"rel-2","listen_count":900,
  "release_date":"2026-09-11","release_group_mbid":"f-3","release_name":"Big","release_group_primary_type":"Album"},
 {"artist_credit_name":"Single Act","caa_id":333,"caa_release_mbid":"rel-3","listen_count":5000,
  "release_date":"2026-09-12","release_group_mbid":"f-4","release_name":"A Single","release_group_primary_type":"Single"}]}}`

type fakeLB struct {
	*httptest.Server
	hits    atomic.Int32
	lbDown  bool
	lastURL atomic.Value
}

func newFakeLB(t *testing.T) *fakeLB {
	t.Helper()
	f := &fakeLB{}
	mux := http.NewServeMux()

	mux.HandleFunc("/1/stats/sitewide/release-groups", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.lastURL.Store(r.URL.String())
		if f.lbDown {
			http.Error(w, "down", 503)
			return
		}
		w.Write([]byte(trendingBody))
	})
	mux.HandleFunc("/1/explore/fresh-releases/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		w.Write([]byte(freshBody))
	})
	// Last.fm's shape, for the fallback.
	mux.HandleFunc("/lastfm", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "" {
			http.Error(w, "no key", 403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tracks": map[string]any{"track": []any{
			map[string]any{"name": "Fallback Song", "artist": map[string]any{"name": "Fallback Act"}},
		}}})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func newClient(t *testing.T, key string) (*Client, *fakeLB) {
	t.Helper()
	f := newFakeLB(t)
	c := New(key)
	c.ListenBrainz = f.URL
	c.LastFM = f.URL + "/lastfm"
	return c, f
}

func TestTrendingAsksForThisWeekAndBuildsDirectCoverURLs(t *testing.T) {
	c, f := newClient(t, "")

	albums, err := c.Trending(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lastURL.Load().(string); !strings.Contains(got, "range=week") {
		t.Errorf("asked for %q; without range=week this is an all-time chart, not trending", got)
	}
	if len(albums) != 3 {
		t.Fatalf("got %d albums, want 3", len(albums))
	}
	if albums[0].Title != "Proof" || albums[0].Artist != "BTS" || albums[0].MBID != "aaa-1" {
		t.Errorf("first album = %+v", albums[0])
	}

	// Plectra's own cache, not archive.org: the artwork host takes four to
	// thirteen seconds a file, and a home page carries thirty-six sleeves.
	want := "/api/cover/mb/bd01f665-425a-44d3-8926-e550417236f5/44649533593"
	if albums[0].Cover != want {
		t.Errorf("cover = %q\nwant %q", albums[0].Cover, want)
	}
	if albums[2].Cover != "" {
		t.Errorf("an entry with no caa_id got a cover URL: %q", albums[2].Cover)
	}
	// Nothing in a chart is in the library, so nothing may claim a local id.
	for _, a := range albums {
		if a.LocalID != 0 {
			t.Errorf("%q carries a local id", a.Title)
		}
	}
}

func TestNewReleasesKeepOnlyAlbumsWithArtworkRankedByListeners(t *testing.T) {
	c, _ := newClient(t, "")

	albums, err := c.New(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 2 {
		t.Fatalf("got %d albums, want 2 — the artless one and the single should be dropped: %+v", len(albums), albums)
	}
	if albums[0].Title != "Big" {
		t.Errorf("first = %q, want the most listened (Big)", albums[0].Title)
	}
	if albums[0].Year != 2026 {
		t.Errorf("year = %d, want 2026 parsed from the release date", albums[0].Year)
	}
}

func TestChartsAreCachedForTheTTL(t *testing.T) {
	c, f := newClient(t, "")
	now := time.Now()
	c.Now = func() time.Time { return now }

	if _, err := c.Trending(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Trending(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Errorf("hit the API %d times inside the TTL, want 1", f.hits.Load())
	}

	now = now.Add(7 * time.Hour) // past the 6h TTL
	if _, err := c.Trending(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Errorf("hits = %d after the TTL expired, want 2", f.hits.Load())
	}
}

func TestLastFMTakesOverWhenListenBrainzIsDown(t *testing.T) {
	c, f := newClient(t, "a-key")
	f.lbDown = true

	albums, err := c.Trending(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Title != "Fallback Song" {
		t.Errorf("albums = %+v, want the Last.fm chart", albums)
	}
}

// Without a key there is no fallback, and saying so beats an empty page with no
// explanation in the log.
func TestNoFallbackWithoutAKey(t *testing.T) {
	c, f := newClient(t, "")
	f.lbDown = true

	if _, err := c.Trending(context.Background(), 3); err == nil {
		t.Fatal("no error when both sources are unavailable")
	}
}
