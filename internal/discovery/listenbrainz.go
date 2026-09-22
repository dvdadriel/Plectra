package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ListenBrainz answers similarity from an open dataset built out of real
// listening. It needs no API key, which is why it is the default provider.
//
// It speaks MusicBrainz ids, so an artist only gets an answer once metadata
// enrichment has found its MBID.
type ListenBrainz struct {
	BaseURL string
	Client  *http.Client
}

func NewListenBrainz() *ListenBrainz {
	return &ListenBrainz{
		BaseURL: "https://labs.api.listenbrainz.org",
		Client:  &http.Client{Timeout: 20 * time.Second},
	}
}

func (l *ListenBrainz) Name() string { return "listenbrainz" }

// The algorithm string selects a precomputed similarity index; this is the one
// ListenBrainz's own radio uses for artists.
const lbAlgorithm = "session_based_days_7500_session_300_contribution_5_threshold_10_limit_100_filter_True_skip_30"

func (l *ListenBrainz) SimilarArtists(ctx context.Context, name, mbid string, limit int) ([]string, error) {
	if mbid == "" {
		// No id, no answer. Saying so is better than guessing by name and
		// returning a different artist who happens to share one.
		return nil, nil
	}
	q := url.Values{"artist_mbids": {mbid}, "algorithm": {lbAlgorithm}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/similar-artists/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listenbrainz: status %d", resp.StatusCode)
	}

	var body []struct {
		Name  string `json:"name"`
		Score int    `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	for _, a := range body {
		if a.Name == "" {
			continue
		}
		out = append(out, a.Name)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
