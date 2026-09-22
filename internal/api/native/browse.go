package native

import (
	"context"
	"net/http"
	"strconv"

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

func (a *API) browseRoutes(mux *http.ServeMux) {
	if a.browser == nil {
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
	albums, err := a.browser.SearchAlbums(r.Context(), q, limit)
	if err != nil {
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
	tracks, err := a.browser.Tracks(r.Context(), id)
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
