package browse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Deezer is the fast half of catalogue search.
//
// MusicBrainz asks clients for one request a second and enforces it, so every
// keystroke queues behind the last — fine for one lookup, useless for a
// search-as-you-type box. Deezer answers in about 200ms with no such rule and
// carries cover art inline. Measured 2026-09-23: album search 0.22s, album
// detail 0.34s, cover from their CDN 0.12s.
//
// This is Deezer's own terms of service rather than open data, and it provides
// no audio either. An album found here is a name and a picture; playing a track
// still goes through the external source registry.
type Deezer struct {
	BaseURL string
	HTTP    *http.Client
}

func NewDeezer() *Deezer {
	return &Deezer{
		BaseURL: "https://api.deezer.com",
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// IDPrefix marks an album id as Deezer's, so one endpoint can serve both
// catalogues without a second parameter to keep in sync.
const IDPrefix = "dz:"

func (d *Deezer) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SearchAlbums finds albums matching free text.
func (d *Deezer) SearchAlbums(ctx context.Context, query string, limit int) ([]Album, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var body struct {
		Data []struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			CoverMedium string `json:"cover_medium"`
			RecordType  string `json:"record_type"`
			Artist      struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	q := url.Values{"q": {query}, "limit": {fmt.Sprint(limit)}}
	if err := d.get(ctx, "/search/album?"+q.Encode(), &body); err != nil {
		return nil, err
	}
	if body.Error != nil {
		return nil, fmt.Errorf("deezer: %s", body.Error.Message)
	}

	out := make([]Album, 0, len(body.Data))
	for _, a := range body.Data {
		out = append(out, Album{
			ID:     fmt.Sprintf("%s%d", IDPrefix, a.ID),
			Title:  a.Title,
			Artist: a.Artist.Name,
			Type:   capitalise(a.RecordType),
			Cover:  a.CoverMedium,
		})
	}
	return out, nil
}

// Tracks lists an album's songs. The album endpoint carries the whole listing,
// so this is one request rather than the release-group dance MusicBrainz needs.
func (d *Deezer) Tracks(ctx context.Context, albumID string) ([]Track, error) {
	id := strings.TrimPrefix(albumID, IDPrefix)
	if id == "" {
		return nil, fmt.Errorf("an album id is required")
	}

	var body struct {
		Artist struct {
			Name string `json:"name"`
		} `json:"artist"`
		Tracks struct {
			Data []struct {
				Title    string `json:"title"`
				Duration int64  `json:"duration"` // seconds, not ms
				Artist   struct {
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"data"`
		} `json:"tracks"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := d.get(ctx, "/album/"+url.PathEscape(id), &body); err != nil {
		return nil, err
	}
	if body.Error != nil {
		return nil, fmt.Errorf("deezer: %s", body.Error.Message)
	}
	if len(body.Tracks.Data) == 0 {
		return nil, fmt.Errorf("no track listing published for this album")
	}

	out := make([]Track, 0, len(body.Tracks.Data))
	for i, t := range body.Tracks.Data {
		artist := t.Artist.Name
		if artist == "" {
			artist = body.Artist.Name
		}
		// Deezer's nested listing carries no track number, but it is in order,
		// so the position is the position.
		out = append(out, Track{
			Title: t.Title, Artist: artist,
			DurationMS: t.Duration * 1000,
			Position:   i + 1,
		})
	}
	return out, nil
}

// Handles reports whether this album id belongs to Deezer.
func (d *Deezer) Handles(albumID string) bool {
	return strings.HasPrefix(albumID, IDPrefix)
}

// capitalise turns Deezer's "album"/"single"/"ep" into the same shape
// MusicBrainz uses. strings.Title is deprecated and does more than this needs.
func capitalise(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
