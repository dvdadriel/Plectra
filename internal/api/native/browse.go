package native

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/plectra/plectra/internal/browse"
)

// Browser searches the world's catalogue, as opposed to the local library.
type Browser interface {
	SearchAlbums(ctx context.Context, query string, limit int) ([]browse.Album, error)
	Tracks(ctx context.Context, albumID string) ([]browse.Track, error)
}

func (a *API) WithBrowser(b Browser) *API {
	a.browser = b
	return a
}

// WithFastBrowser adds the catalogue used for search-as-you-type. It answers in
// about 200ms with artwork inline; the slower one stays as the fallback and for
// ids it owns.
func (a *API) WithFastBrowser(b Browser) *API {
	a.fastBrowser = b
	return a
}

// searchers returns the catalogues to try, fastest first.
func (a *API) searchers() []Browser {
	var out []Browser
	if a.fastBrowser != nil {
		out = append(out, a.fastBrowser)
	}
	if a.browser != nil {
		out = append(out, a.browser)
	}
	return out
}

func (a *API) browseRoutes(mux *http.ServeMux) {
	if a.browser == nil && a.fastBrowser == nil {
		return
	}
	mux.HandleFunc("GET /api/browse", a.browseSearch)
	mux.HandleFunc("GET /api/browse/album", a.browseAlbum)
}

func (a *API) browseSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q is required", 400)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	// The fast catalogue first; the slower one only if it answered with nothing
	// or could not be reached. One of them being down is not a failed search.
	var (
		albums []browse.Album
		err    error
	)
	for _, b := range a.searchers() {
		albums, err = b.SearchAlbums(r.Context(), q, limit)
		if err == nil && len(albums) > 0 {
			break
		}
	}
	if err != nil && len(albums) == 0 {
		http.Error(w, err.Error(), 502)
		return
	}
	if albums == nil {
		albums = []browse.Album{}
	}
	writeJSON(w, albums)
}

// browseAlbum returns the track listing, marking which songs are already in the
// library so the view can say what will play from disk and what will not.
func (a *API) browseAlbum(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", 400)
		return
	}
	// The id says which catalogue it came from, so a listing is never asked of
	// the wrong one.
	src := a.browser
	if a.fastBrowser != nil && strings.HasPrefix(id, browse.IDPrefix) {
		src = a.fastBrowser
	}
	if src == nil {
		http.Error(w, "that catalogue is not configured", 400)
		return
	}
	tracks, err := src.Tracks(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	pairs := make([][2]string, len(tracks))
	for i, t := range tracks {
		pairs[i] = [2]string{t.Artist, t.Title}
	}
	local, err := a.lists.MatchTracks(r.Context(), pairs)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	type entry struct {
		browse.Track
		LocalID int64 `json:"localId,omitempty"`
	}
	out := make([]entry, len(tracks))
	for i, t := range tracks {
		out[i] = entry{Track: t, LocalID: local[i]}
	}
	writeJSON(w, out)
}
