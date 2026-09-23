// Package chart reads what the wider world is listening to, as opposed to what
// this library holds.
//
// ListenBrainz first: no key, open data, and its sitewide statistics are already
// album-shaped, with Cover Art Archive ids attached. Last.fm is the fallback and
// needs a free key; it answers in artists and tracks, so an album section built
// from it is thinner.
//
// Nothing here is playable on its own. A chart album is a name and a picture;
// pressing play sends it through the same external-source path a catalogue
// search uses.
package chart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// Album is one entry in an album row. Exactly one identity is set: LocalID for
// something in the library, MBID for something only the wider catalogue knows.
// The view needs that distinction — one opens a track listing from disk, the
// other has to be looked up before anything can play.
type Album struct {
	LocalID int64  `json:"id,omitempty"`
	MBID    string `json:"mbid,omitempty"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	// Cover is a path on Plectra, not on archive.org: the artwork host answers
	// in four to thirteen seconds, so it is fetched once and cached locally.
	Cover string `json:"cover,omitempty"`
	Year  int    `json:"year,omitempty"`
	// HasCover mirrors the library's flag so one card template serves both.
	HasCover bool `json:"hasCover,omitempty"`
}

type Client struct {
	HTTP *http.Client
	Now  func() time.Time

	// Injectable so tests need no network.
	ListenBrainz string
	LastFM       string
	LastFMKey    string

	// TTL is how long a chart is reused. These move daily at most; a request
	// per page load would be rude to a free service and slower for the user.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	albums []Album
	at     time.Time
}

func New(lastFMKey string) *Client {
	return &Client{
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		Now:          time.Now,
		ListenBrainz: "https://api.listenbrainz.org",
		LastFM:       "https://ws.audioscrobbler.com/2.0/",
		LastFMKey:    lastFMKey,
		TTL:          6 * time.Hour,
		cache:        map[string]entry{},
	}
}

const userAgent = "Plectra/0.9 (https://github.com/plectra/plectra)"

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// cached runs fetch unless a recent answer is already held.
func (c *Client) cached(key string, fetch func() ([]Album, error)) ([]Album, error) {
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && c.Now().Sub(e.at) < c.TTL {
		c.mu.Unlock()
		return e.albums, nil
	}
	c.mu.Unlock()

	albums, err := fetch()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cache[key] = entry{albums: albums, at: c.Now()}
	c.mu.Unlock()
	return albums, nil
}

// coverURL points at Plectra's own cache rather than at the Cover Art Archive.
// Measured on 2026-09-23: archive.org takes four to thirteen seconds a file and
// five even warm, and a home page carries thirty-six sleeves. Plectra fetches
// each one once and serves it from disk afterwards.
func coverURL(releaseMBID string, caaID int64) string {
	if releaseMBID == "" || caaID == 0 {
		return ""
	}
	return fmt.Sprintf("/api/cover/mb/%s/%d", releaseMBID, caaID)
}

// Trending is what the world played most this past week.
func (c *Client) Trending(ctx context.Context, limit int) ([]Album, error) {
	return c.cached("trending", func() ([]Album, error) {
		var body struct {
			Payload struct {
				ReleaseGroups []struct {
					Name           string `json:"release_group_name"`
					Artist         string `json:"artist_name"`
					MBID           string `json:"release_group_mbid"`
					CAAID          int64  `json:"caa_id"`
					CAAReleaseMBID string `json:"caa_release_mbid"`
					ListenCount    int    `json:"listen_count"`
				} `json:"release_groups"`
			} `json:"payload"`
		}
		u := fmt.Sprintf("%s/1/stats/sitewide/release-groups?count=%d&range=week",
			c.ListenBrainz, limit)
		if err := c.getJSON(ctx, u, &body); err != nil {
			return c.lastFMTop(ctx, limit)
		}

		out := make([]Album, 0, len(body.Payload.ReleaseGroups))
		for _, rg := range body.Payload.ReleaseGroups {
			out = append(out, Album{
				MBID: rg.MBID, Title: rg.Name, Artist: rg.Artist,
				Cover: coverURL(rg.CAAReleaseMBID, rg.CAAID),
			})
		}
		if len(out) == 0 {
			return c.lastFMTop(ctx, limit)
		}
		return out, nil
	})
}

// New is what came out recently. The endpoint answers with everything released
// in the window, most of it with no artwork and no listeners at all, so it is
// ranked and trimmed here rather than shown raw.
func (c *Client) New(ctx context.Context, limit int) ([]Album, error) {
	return c.cached("new", func() ([]Album, error) {
		var body struct {
			Payload struct {
				Releases []struct {
					Name           string `json:"release_name"`
					Artist         string `json:"artist_credit_name"`
					MBID           string `json:"release_group_mbid"`
					CAAID          int64  `json:"caa_id"`
					CAAReleaseMBID string `json:"caa_release_mbid"`
					ListenCount    int    `json:"listen_count"`
					Date           string `json:"release_date"`
					Type           string `json:"release_group_primary_type"`
				} `json:"releases"`
			} `json:"payload"`
		}
		// The trailing slash matters: without it the API answers 308.
		u := c.ListenBrainz + "/1/explore/fresh-releases/?days=14&sort=release_date"
		if err := c.getJSON(ctx, u, &body); err != nil {
			return nil, err
		}

		rs := body.Payload.Releases
		// Artwork is the filter that matters: an entry with no cover is one
		// nobody has catalogued, and a grid of blank sleeves is not a section.
		kept := rs[:0]
		for _, r := range rs {
			if r.CAAID != 0 && r.Type == "Album" {
				kept = append(kept, r)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool {
			return kept[i].ListenCount > kept[j].ListenCount
		})
		if len(kept) > limit {
			kept = kept[:limit]
		}

		out := make([]Album, 0, len(kept))
		for _, r := range kept {
			out = append(out, Album{
				MBID: r.MBID, Title: r.Name, Artist: r.Artist,
				Cover: coverURL(r.CAAReleaseMBID, r.CAAID),
				Year:  year(r.Date),
			})
		}
		return out, nil
	})
}

// lastFMTop is the fallback. Last.fm charts tracks and artists, not albums, so
// what comes back is the top tracks presented as what they are.
func (c *Client) lastFMTop(ctx context.Context, limit int) ([]Album, error) {
	if c.LastFMKey == "" {
		return nil, fmt.Errorf("listenbrainz is unreachable and no Last.fm key is set")
	}
	var body struct {
		Tracks struct {
			Track []struct {
				Name   string `json:"name"`
				Artist struct {
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"track"`
		} `json:"tracks"`
	}
	q := url.Values{
		"method":  {"chart.getTopTracks"},
		"api_key": {c.LastFMKey},
		"format":  {"json"},
		"limit":   {fmt.Sprint(limit)},
	}
	if err := c.getJSON(ctx, c.LastFM+"?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(body.Tracks.Track))
	for _, t := range body.Tracks.Track {
		out = append(out, Album{Title: t.Name, Artist: t.Artist.Name})
	}
	return out, nil
}

func year(date string) int {
	if len(date) < 4 {
		return 0
	}
	y := 0
	for _, r := range date[:4] {
		if r < '0' || r > '9' {
			return 0
		}
		y = y*10 + int(r-'0')
	}
	return y
}
