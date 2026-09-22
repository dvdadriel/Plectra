package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// LastFM answers similarity by artist name, which covers the artists
// MusicBrainz enrichment has not reached yet. It needs a free API key, so it is
// absent unless the user supplies one.
type LastFM struct {
	BaseURL string
	Client  *http.Client

	mu     sync.RWMutex
	apiKey string
}

// NewLastFM always returns a provider. The key can be supplied later from the
// UI, so the provider exists from the start and simply answers nothing until
// there is one.
func NewLastFM(apiKey string) *LastFM {
	return &LastFM{
		apiKey:  apiKey,
		BaseURL: "https://ws.audioscrobbler.com/2.0/",
		Client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (l *LastFM) SetKey(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apiKey = key
}

func (l *LastFM) Key() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.apiKey
}

func (l *LastFM) Name() string { return "lastfm" }

func (l *LastFM) SimilarArtists(ctx context.Context, name, mbid string, limit int) ([]string, error) {
	key := l.Key()
	if key == "" {
		return nil, nil // no key, no answer — not an error
	}
	q := url.Values{
		"method":  {"artist.getsimilar"},
		"api_key": {key},
		"format":  {"json"},
		"limit":   {fmt.Sprint(limit)},
	}
	// Prefer the id when the library has one: names are ambiguous, ids are not.
	if mbid != "" {
		q.Set("mbid", mbid)
	} else {
		q.Set("artist", name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("last.fm: status %d", resp.StatusCode)
	}

	var body struct {
		SimilarArtists struct {
			Artist []struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"similarartists"`
		Error   int    `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Error != 0 {
		return nil, fmt.Errorf("last.fm: %s", body.Message)
	}
	out := make([]string, 0, len(body.SimilarArtists.Artist))
	for _, a := range body.SimilarArtists.Artist {
		if a.Name != "" {
			out = append(out, a.Name)
		}
	}
	return out, nil
}
