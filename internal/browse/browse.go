// Package browse searches the world's music catalogue, as opposed to the user's
// own library.
//
// It reads MusicBrainz, which needs no key, has no result cap, and carries full
// track listings with durations — the three things Spotify cannot give without
// an account. Nothing here provides audio: a track found this way is handed to
// the source registry to be played.
package browse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Album struct {
	ID     string `json:"id"` // MusicBrainz release-group id, or "dz:<n>" for Deezer
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Year   int    `json:"year,omitempty"`
	Type   string `json:"type,omitempty"` // Album, Single, EP…
	// Cover is set when the catalogue carries artwork inline. MusicBrainz does
	// not; the caller falls back to Plectra's release-group cover cache.
	Cover string `json:"cover,omitempty"`
}

type Track struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMS int64  `json:"durationMs"`
	Position   int    `json:"position"`
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	// MinInterval is MusicBrainz's one-request-per-second rule, which is
	// enforced on their side: exceeding it gets the client blocked.
	MinInterval time.Duration

	last time.Time
}

func New() *Client {
	return &Client{
		BaseURL:     "https://musicbrainz.org/ws/2",
		HTTP:        &http.Client{Timeout: 20 * time.Second},
		MinInterval: time.Second,
	}
}

const userAgent = "Plectra/0.9 (https://github.com/plectra/plectra)"

func (c *Client) get(ctx context.Context, path string, out any) error {
	if wait := c.MinInterval - time.Since(c.last); wait > 0 && !c.last.IsZero() {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
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
		return fmt.Errorf("musicbrainz answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SearchAlbums finds releases matching free text: an artist, an album, or both.
func (c *Client) SearchAlbums(ctx context.Context, query string, limit int) ([]Album, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var body struct {
		ReleaseGroups []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			FirstRelease string `json:"first-release-date"`
			PrimaryType  string `json:"primary-type"`
			Score        int    `json:"score"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"release-groups"`
	}
	path := fmt.Sprintf("/release-group?query=%s&fmt=json&limit=%d", url.QueryEscape(query), limit)
	if err := c.get(ctx, path, &body); err != nil {
		return nil, err
	}

	out := make([]Album, 0, len(body.ReleaseGroups))
	for _, rg := range body.ReleaseGroups {
		a := Album{ID: rg.ID, Title: rg.Title, Year: year(rg.FirstRelease), Type: rg.PrimaryType}
		if len(rg.ArtistCredit) > 0 {
			a.Artist = rg.ArtistCredit[0].Name
		}
		out = append(out, a)
	}
	return out, nil
}

// Tracks lists an album's songs. MusicBrainz models an album as a release group
// with several releases (editions, reissues); the one with the most tracks is
// the most useful answer, since a single-track promo edition helps nobody.
func (c *Client) Tracks(ctx context.Context, releaseGroupID string) ([]Track, error) {
	if releaseGroupID == "" {
		return nil, fmt.Errorf("an album id is required")
	}

	var body struct {
		Releases []struct {
			Title        string `json:"title"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
			Media []struct {
				Tracks []struct {
					Title    string `json:"title"`
					Length   int64  `json:"length"`
					Position int    `json:"position"`
				} `json:"tracks"`
			} `json:"media"`
		} `json:"releases"`
	}
	path := "/release?release-group=" + url.QueryEscape(releaseGroupID) +
		"&inc=recordings+artist-credits&fmt=json&limit=10"
	if err := c.get(ctx, path, &body); err != nil {
		return nil, err
	}

	best := -1
	bestCount := 0
	for i, r := range body.Releases {
		n := 0
		for _, m := range r.Media {
			n += len(m.Tracks)
		}
		if n > bestCount {
			best, bestCount = i, n
		}
	}
	if best < 0 || bestCount == 0 {
		return nil, fmt.Errorf("no track listing published for this album")
	}

	r := body.Releases[best]
	artist := ""
	if len(r.ArtistCredit) > 0 {
		artist = r.ArtistCredit[0].Name
	}

	var out []Track
	for _, m := range r.Media {
		for _, t := range m.Tracks {
			out = append(out, Track{
				Title: t.Title, Artist: artist,
				DurationMS: t.Length, Position: t.Position,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
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
