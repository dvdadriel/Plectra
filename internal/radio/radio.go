// Package radio browses internet radio directories and turns stations into
// something the player can open.
//
// Radio is the second audio source Plectra has: full-length music, legal to
// play — stations hold the broadcast licences — and it needs no account, no key
// and nothing downloaded.
package radio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Station is one entry in the directory, reduced to what is actually used.
type Station struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Codec   string `json:"codec"`
	Bitrate int    `json:"bitrate"`
	Country string `json:"country"`
	Tags    string `json:"tags"`
	Favicon string `json:"favicon,omitempty"`
}

type Directory struct {
	BaseURL string
	Client  *http.Client
}

func New() *Directory {
	return &Directory{
		// all.api.radio-browser.info is round-robin DNS across the mirrors.
		BaseURL: "https://all.api.radio-browser.info",
		Client:  &http.Client{Timeout: 20 * time.Second},
	}
}

// userAgent identifies Plectra to the directory, which the project asks of
// clients so it can see what is using it.
const userAgent = "Plectra/0.7 (https://github.com/plectra/plectra)"

// Top returns the most-clicked stations, for a browse view that is not empty
// before the user has typed anything. The second return says whether the
// directory held more beyond this page.
func (d *Directory) Top(ctx context.Context, limit, offset int) ([]Station, bool, error) {
	n := clamp(limit)
	q := url.Values{
		"limit":      {fmt.Sprint(n)},
		"offset":     {fmt.Sprint(max(offset, 0))},
		"hidebroken": {"true"},
	}
	return d.fetch(ctx, "/json/stations/topclick?"+q.Encode(), n)
}

// Search finds stations by name.
func (d *Directory) Search(ctx context.Context, query string, limit, offset int) ([]Station, bool, error) {
	n := clamp(limit)
	q := url.Values{
		"name":       {query},
		"limit":      {fmt.Sprint(n)},
		"offset":     {fmt.Sprint(max(offset, 0))},
		"hidebroken": {"true"},
		"order":      {"clickcount"},
		"reverse":    {"true"},
	}
	return d.fetch(ctx, "/json/stations/search?"+q.Encode(), n)
}

// Click tells the directory a station was played. It is how the rankings stay
// meaningful, and the project asks clients to send it.
func (d *Directory) Click(ctx context.Context, uuid string) {
	if uuid == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"/json/url/"+uuid, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	if resp, err := d.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// fetch reads one page. `asked` is the page size, which is how a full page is
// told from the last one: the filtering below drops unplayable stations, so the
// number returned says nothing about whether more exist.
func (d *Directory) fetch(ctx context.Context, path string, asked int) ([]Station, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("radio directory: status %d", resp.StatusCode)
	}

	var raw []struct {
		UUID    string `json:"stationuuid"`
		Name    string `json:"name"`
		URL     string `json:"url_resolved"`
		Codec   string `json:"codec"`
		Bitrate int    `json:"bitrate"`
		Country string `json:"country"`
		Tags    string `json:"tags"`
		Favicon string `json:"favicon"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, false, err
	}

	out := make([]Station, 0, len(raw))
	for _, s := range raw {
		if s.URL == "" || !Playable(s.Codec) {
			// Plectra decodes MP3 and Ogg in pure Go. An AAC station would be
			// listed and then refuse to play, which is worse than not listing it.
			continue
		}
		out = append(out, Station{
			UUID: s.UUID, Name: strings.TrimSpace(s.Name), URL: s.URL,
			Codec: s.Codec, Bitrate: s.Bitrate, Country: s.Country,
			Tags: s.Tags, Favicon: s.Favicon,
		})
	}
	return out, len(raw) >= asked, nil
}

// Playable reports whether Plectra can decode a station's codec today.
func Playable(codec string) bool {
	switch strings.ToUpper(strings.TrimSpace(codec)) {
	case "MP3", "OGG", "VORBIS":
		return true
	}
	return false
}

func clamp(n int) int {
	if n <= 0 || n > 100 {
		return 30
	}
	return n
}
