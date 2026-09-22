package native

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/plectra/plectra/internal/metadata"
	"github.com/plectra/plectra/internal/store"
)

// Enricher is the slice of the metadata worker the API is allowed to use.
type Enricher interface {
	Seed(ctx context.Context) (int, error)
	Reset(ctx context.Context, provider string) error
	Stats(ctx context.Context) (store.JobStats, error)
}

// SpotifyLink is the slice of the Spotify provider the API is allowed to use.
type SpotifyLink interface {
	Configured() bool
	// CredentialsLookValid reports whether the configured client id and secret
	// have the shape Spotify issues. False means the login will fail before the
	// user ever sees a consent screen.
	CredentialsLookValid() bool
	Linked(ctx context.Context) bool
	StartAuth() string
	CheckState(state string) bool
	Exchange(ctx context.Context, code string) error
	ImportPlaylists(ctx context.Context) (metadata.ImportResult, error)
	ImportLiked(ctx context.Context) (metadata.ImportResult, error)
}

func (a *API) enrichRoutes(mux *http.ServeMux) {
	if a.enrich != nil {
		mux.HandleFunc("GET /api/enrich", a.enrichStats)
		mux.HandleFunc("POST /api/enrich", a.enrichRun)
	}
	// Spotify routes exist even without credentials, so the UI can explain what
	// is missing instead of showing a bare 404.
	mux.HandleFunc("GET /api/spotify/status", a.spotifyStatus)
	mux.HandleFunc("GET /api/spotify/login", a.spotifyLogin)
	mux.HandleFunc("GET /api/spotify/callback", a.spotifyCallback)
	mux.HandleFunc("POST /api/spotify/import", a.spotifyImport)
}

func (a *API) enrichStats(w http.ResponseWriter, r *http.Request) {
	st, err := a.enrich.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, st)
}

// enrichRun queues work. force=1 re-runs entities that already succeeded.
func (a *API) enrichRun(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("force") == "1" {
		if err := a.enrich.Reset(r.Context(), r.URL.Query().Get("provider")); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	n, err := a.enrich.Seed(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"queued": n})
}

// unconfigured reports that Spotify cannot be used, and says why.
func (a *API) unconfigured(w http.ResponseWriter) bool {
	if a.spotify == nil || !a.spotify.Configured() {
		http.Error(w, "set -spotify-id and -spotify-secret to use Spotify", 503)
		return true
	}
	return false
}

// spotifyStatus lets the UI say what is actually true rather than guessing.
func (a *API) spotifyStatus(w http.ResponseWriter, r *http.Request) {
	configured := a.spotify != nil && a.spotify.Configured()
	linked := configured && a.spotify.Linked(r.Context())
	valid := configured && a.spotify.CredentialsLookValid()
	writeJSON(w, map[string]bool{"configured": configured, "linked": linked, "credentialsValid": valid})
}

func (a *API) spotifyLogin(w http.ResponseWriter, r *http.Request) {
	if a.unconfigured(w) {
		return
	}
	url := a.spotify.StartAuth()
	if url == "" {
		http.Error(w, "could not start the Spotify login", 500)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func (a *API) spotifyCallback(w http.ResponseWriter, r *http.Request) {
	if a.unconfigured(w) {
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "spotify refused: "+e, 400)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", 400)
		return
	}
	// The state must be the one this server issued for this login.
	if !a.spotify.CheckState(r.URL.Query().Get("state")) {
		http.Error(w, "this login did not start here; open Settings and try again", 400)
		return
	}
	if err := a.spotify.Exchange(r.Context(), code); err != nil {
		log.Printf("spotify: exchange: %v", err)
		http.Error(w, "could not complete the Spotify login", 502)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// spotifyImport pulls playlists and liked songs into the local library.
func (a *API) spotifyImport(w http.ResponseWriter, r *http.Request) {
	if a.unconfigured(w) {
		return
	}
	what := r.URL.Query().Get("what")
	var (
		res metadata.ImportResult
		err error
	)
	switch what {
	case "liked":
		res, err = a.spotify.ImportLiked(r.Context())
	case "playlists", "":
		res, err = a.spotify.ImportPlaylists(r.Context())
	default:
		http.Error(w, "what must be playlists or liked", 400)
		return
	}
	if errors.Is(err, metadata.ErrNotAuthorized) {
		http.Error(w, "link your Spotify account first", 401)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	writeJSON(w, res)
}
