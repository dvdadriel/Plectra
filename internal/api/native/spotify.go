package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/spotify"
)

// SpotifyReader reads public Spotify playlists. No account is involved.
type SpotifyReader interface {
	Fetch(ctx context.Context, idOrURL string) (*spotify.Playlist, error)
}

func (a *API) WithSpotify(s SpotifyReader) *API {
	a.spotify = s
	return a
}

func (a *API) spotifyRoutes(mux *http.ServeMux) {
	if a.spotify == nil {
		return
	}
	mux.HandleFunc("POST /api/spotify/playlist", a.spotifyPlaylist)
}

// spotifyPlaylist reads a public playlist and says, per track, whether the file
// is already in the library. Tracks that are not can still be played, from an
// external source, one at a time.
func (a *API) spotifyPlaylist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decode(r, &req); err != nil || req.URL == "" {
		http.Error(w, "a Spotify playlist link is required", 400)
		return
	}
	pl, err := a.spotify.Fetch(r.Context(), req.URL)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	local, err := a.lists.MatchTracks(r.Context(), toMatchable(pl.Tracks))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	type entry struct {
		spotify.Track
		LocalID int64 `json:"localId,omitempty"`
	}
	out := struct {
		Name      string  `json:"name"`
		ID        string  `json:"id"`
		CoverURL  string  `json:"coverUrl,omitempty"`
		Truncated bool    `json:"truncated"`
		InLibrary int     `json:"inLibrary"`
		Tracks    []entry `json:"tracks"`
	}{Name: pl.Name, ID: pl.ID, CoverURL: pl.CoverURL, Truncated: pl.Truncated}

	for i, t := range pl.Tracks {
		e := entry{Track: t, LocalID: local[i]}
		if e.LocalID != 0 {
			out.InLibrary++
		}
		out.Tracks = append(out.Tracks, e)
	}
	writeJSON(w, out)
}

func toMatchable(tracks []spotify.Track) [][2]string {
	out := make([][2]string, len(tracks))
	for i, t := range tracks {
		out[i] = [2]string{t.Artist, t.Title}
	}
	return out
}
