package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MusicBrainz is the canonical metadata source. Spotify only supplements it.
type MusicBrainz struct {
	BaseURL string
	Client  *http.Client
	lim     *limiter
}

// NewMusicBrainz builds the provider. The one-request-per-second limit is not
// advisory: MusicBrainz blocks clients that ignore it.
func NewMusicBrainz(clock Clock) *MusicBrainz {
	return &MusicBrainz{
		BaseURL: "https://musicbrainz.org/ws/2",
		Client:  &http.Client{Timeout: 15 * time.Second},
		lim:     newLimiter(clock, time.Second),
	}
}

func (m *MusicBrainz) Name() string { return "musicbrainz" }

// userAgent is required by MusicBrainz; requests without a real one are refused.
const userAgent = "Plectra/0.3 (https://github.com/plectra/plectra)"

func (m *MusicBrainz) Lookup(ctx context.Context, q Query) ([]Match, error) {
	if err := m.lim.wait(ctx); err != nil {
		return nil, err
	}

	var endpoint, query string
	switch q.Kind {
	case KindArtist:
		endpoint, query = "artist", `artist:"`+escape(q.Artist)+`"`
	case KindAlbum:
		endpoint = "release-group"
		query = `releasegroup:"` + escape(q.Album) + `" AND artist:"` + escape(q.Artist) + `"`
	default:
		endpoint = "recording"
		query = `recording:"` + escape(q.Title) + `" AND artist:"` + escape(q.Artist) + `"`
	}

	u := fmt.Sprintf("%s/%s?query=%s&fmt=json&limit=5", m.BaseURL, endpoint, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RetryableError{After: retryAfter(resp), Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musicbrainz: status %d", resp.StatusCode)
	}

	var body struct {
		Artists []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Score int    `json:"score"`
		} `json:"artists"`
		ReleaseGroups []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			FirstRelease string `json:"first-release-date"`
			Score        int    `json:"score"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"release-groups"`
		Recordings []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			Score        int    `json:"score"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"recordings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	var out []Match
	for _, a := range body.Artists {
		out = append(out, Match{ID: a.ID, Artist: a.Name, Score: a.Score})
	}
	for _, rg := range body.ReleaseGroups {
		mt := Match{ID: rg.ID, Album: rg.Title, Year: year(rg.FirstRelease), Score: rg.Score}
		if len(rg.ArtistCredit) > 0 {
			mt.Artist = rg.ArtistCredit[0].Name
		}
		// Cover Art Archive is addressed by release-group id, so no extra lookup.
		mt.CoverURL = "https://coverartarchive.org/release-group/" + rg.ID + "/front-500"
		out = append(out, mt)
	}
	for _, r := range body.Recordings {
		mt := Match{ID: r.ID, Title: r.Title, Score: r.Score}
		if len(r.ArtistCredit) > 0 {
			mt.Artist = r.ArtistCredit[0].Name
		}
		out = append(out, mt)
	}
	return out, nil
}

// escape keeps user data from being read as Lucene syntax by the search server.
func escape(s string) string {
	r := strings.NewReplacer(`"`, ` `, `\`, ` `, `:`, ` `, `(`, ` `, `)`, ` `, `~`, ` `, `^`, ` `)
	return strings.TrimSpace(r.Replace(s))
}

func year(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

// RetryableError means the provider asked us to come back later. The worker
// honours After rather than inventing its own delay.
type RetryableError struct {
	After  time.Duration
	Status int
}

func (e *RetryableError) Error() string {
	return fmt.Sprintf("provider busy (status %d), retry in %s", e.Status, e.After)
}

func retryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}
