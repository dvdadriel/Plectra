// Package spotify reads public Spotify playlists without an account.
//
// It uses the embed page, which Spotify renders server-side for anyone — the
// same data a website gets when it embeds a playlist. No login, no developer
// application, no token, nothing stored.
//
// Two limits come with that, and both are Spotify's, not ours:
//   - at most 25 tracks per playlist; the embed carries no paging information
//   - public playlists only; a private one returns nothing at all
//
// For longer or private playlists, docs/spotify-export.js reads the full list
// from the user's own logged-in browser instead.
package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// EmbedLimit is what the embed page returns, no matter how long the playlist is.
const EmbedLimit = 25

type Playlist struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	CoverURL string  `json:"coverUrl,omitempty"`
	Tracks   []Track `json:"tracks"`
	// Truncated says the playlist has more tracks than Spotify handed over.
	Truncated bool `json:"truncated"`
}

type Track struct {
	SpotifyID  string `json:"spotifyId"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMS int64  `json:"durationMs"`
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func New() *Client {
	return &Client{
		BaseURL: "https://open.spotify.com",
		HTTP:    &http.Client{Timeout: 25 * time.Second},
	}
}

var idPattern = regexp.MustCompile(`([A-Za-z0-9]{22})`)

// ParseID accepts a playlist URL, a spotify: URI, or a bare id.
func ParseID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("give a Spotify playlist link or id")
	}
	if strings.Contains(input, "/track/") || strings.Contains(input, ":track:") {
		return "", fmt.Errorf("that is a track link; give a playlist link")
	}
	m := idPattern.FindString(input)
	if m == "" {
		return "", fmt.Errorf("no playlist id found in %q", input)
	}
	return m, nil
}

// Fetch reads a public playlist. A private one is reported as such rather than
// as an empty playlist, because the difference matters to the user.
func (c *Client) Fetch(ctx context.Context, idOrURL string) (*Playlist, error) {
	id, err := ParseID(idOrURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/embed/playlist/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Plectra/0.8")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify answered %d for that playlist", resp.StatusCode)
	}

	// 2MB is far more than an embed page; the cap keeps a wrong URL from
	// pulling something enormous into memory.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	return parse(string(b), id)
}

// parse pulls the playlist out of the page's embedded state.
func parse(html, id string) (*Playlist, error) {
	m := regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`).FindStringSubmatch(html)
	if len(m) < 2 {
		return nil, fmt.Errorf("this page carries no playlist data; Spotify may have changed the embed")
	}

	var page struct {
		Props struct {
			PageProps struct {
				State struct {
					Data struct {
						Entity struct {
							Name     string `json:"name"`
							Type     string `json:"type"`
							CoverArt struct {
								Sources []struct {
									URL string `json:"url"`
								} `json:"sources"`
							} `json:"coverArt"`
							TrackList []struct {
								URI      string `json:"uri"`
								Title    string `json:"title"`
								Subtitle string `json:"subtitle"`
								Duration int64  `json:"duration"`
							} `json:"trackList"`
						} `json:"entity"`
					} `json:"data"`
				} `json:"state"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal([]byte(m[1]), &page); err != nil {
		return nil, err
	}

	ent := page.Props.PageProps.State.Data.Entity
	if ent.Name == "" && len(ent.TrackList) == 0 {
		return nil, fmt.Errorf("that playlist is private or does not exist — " +
			"only public playlists can be read without an account")
	}

	pl := &Playlist{ID: id, Name: ent.Name, Truncated: len(ent.TrackList) >= EmbedLimit}
	if len(ent.CoverArt.Sources) > 0 {
		pl.CoverURL = ent.CoverArt.Sources[0].URL
	}
	for _, t := range ent.TrackList {
		pl.Tracks = append(pl.Tracks, Track{
			SpotifyID:  strings.TrimPrefix(t.URI, "spotify:track:"),
			Title:      t.Title,
			Artist:     t.Subtitle,
			DurationMS: t.Duration,
		})
	}
	return pl, nil
}
