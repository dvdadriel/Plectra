package native

import (
	"context"
	"net/http"
	"net/url"

	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/spotify"
)

// SpotifyAccount is the slice of the Spotify link the API is allowed to use.
type SpotifyAccount interface {
	Configured() bool
	AuthURL() string
	Exchange(ctx context.Context, state, code string) error
	Connected(ctx context.Context) (bool, string)
	Disconnect(ctx context.Context) error
	Playlists(ctx context.Context) ([]spotify.Playlist, []string, error)
	Liked(ctx context.Context) ([]spotify.Track, error)
}

func (a *API) WithSpotify(s SpotifyAccount) *API {
	a.spotify = s
	return a
}

func (a *API) spotifyRoutes(mux *http.ServeMux) {
	if a.spotify == nil {
		return
	}
	mux.HandleFunc("GET /api/spotify", a.spotifyStatus)
	mux.HandleFunc("DELETE /api/spotify", a.spotifyDisconnect)
	mux.HandleFunc("GET /api/spotify/login", a.spotifyLogin)
	mux.HandleFunc("GET /api/spotify/callback", a.spotifyCallback)
	mux.HandleFunc("POST /api/spotify/import", a.spotifyImport)
}

func (a *API) spotifyStatus(w http.ResponseWriter, r *http.Request) {
	connected, who := a.spotify.Connected(r.Context())
	writeJSON(w, map[string]any{
		"configured": a.spotify.Configured(),
		"connected":  connected,
		"account":    who,
	})
}

// spotifyLogin sends the browser to Spotify. A redirect rather than a JSON URL:
// the consent screen has to be a top-level navigation, not a fetch.
func (a *API) spotifyLogin(w http.ResponseWriter, r *http.Request) {
	if !a.spotify.Configured() {
		http.Error(w, "no Spotify client id is configured", 400)
		return
	}
	http.Redirect(w, r, a.spotify.AuthURL(), http.StatusFound)
}

// spotifyCallback is where Spotify sends the browser back. It always lands the
// user on the Settings panel — an error belongs in the UI they started from,
// not on a blank page.
func (a *API) spotifyCallback(w http.ResponseWriter, r *http.Request) {
	// Escaped: an outcome carries Spotify's own wording, and a raw space in a
	// Location header is not a valid header at all.
	back := func(outcome string) {
		http.Redirect(w, r, "/#settings&spotify="+url.QueryEscape(outcome), http.StatusFound)
	}

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		back(e)
		return
	}
	if err := a.spotify.Exchange(r.Context(), q.Get("state"), q.Get("code")); err != nil {
		back(err.Error())
		return
	}
	back("connected")
}

func (a *API) spotifyDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := a.spotify.Disconnect(r.Context()); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.spotifyStatus(w, r)
}

// spotifyImport pulls the account's playlists and saved songs and turns them
// into local ones. The same import path the browser script uses, so the result
// is reported the same way — including what could not be matched.
func (a *API) spotifyImport(w http.ResponseWriter, r *http.Request) {
	lists, refused, err := a.spotify.Playlists(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	liked, err := a.spotify.Liked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	exp := playlist.Export{Source: "spotify"}
	for _, pl := range lists {
		out := playlist.ExportPlaylist{Name: pl.Name, SpotifyID: pl.ID}
		for _, t := range pl.Tracks {
			out.Tracks = append(out.Tracks, exportTrack(t))
		}
		exp.Playlists = append(exp.Playlists, out)
	}
	for _, t := range liked {
		exp.Liked = append(exp.Liked, exportTrack(t))
	}

	res, err := a.lists.Import(r.Context(), exp)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, struct {
		playlist.ImportResult
		// Spotify's own editorial lists are refused to apps registered after
		// November 2024. Naming them beats a total that quietly does not add up.
		Refused []string `json:"refused,omitempty"`
	}{res, refused})
}

func exportTrack(t spotify.Track) playlist.ExportTrack {
	return playlist.ExportTrack{Name: t.Name, Artists: t.Artists, Album: t.Album}
}
