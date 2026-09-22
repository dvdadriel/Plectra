package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/source"
	"github.com/plectra/plectra/internal/store"
)

// Sources is the slice of the external-source registry the API may use.
type Sources interface {
	Names() []string
	Find(ctx context.Context, q source.Query) ([]source.Candidate, error)
	Resolve(ctx context.Context, c source.Candidate) (string, error)
}

func (a *API) WithSources(s Sources) *API {
	a.sources = s
	return a
}

func (a *API) sourceRoutes(mux *http.ServeMux) {
	if a.sources == nil {
		return
	}
	mux.HandleFunc("GET /api/source", a.sourceStatus)
	mux.HandleFunc("GET /api/source/find", a.sourceFind)
	mux.HandleFunc("POST /api/source/play", a.sourcePlay)
}

func (a *API) sourceStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"providers": a.sources.Names()})
}

func (a *API) sourceFind(w http.ResponseWriter, r *http.Request) {
	q := source.Query{
		Artist: r.URL.Query().Get("artist"),
		Title:  r.URL.Query().Get("title"),
	}
	if q.String() == "" {
		http.Error(w, "artist or title is required", 400)
		return
	}
	found, err := a.sources.Find(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if found == nil {
		found = []source.Candidate{}
	}
	writeJSON(w, found)
}

// sourcePlay resolves a candidate and plays it straight away. The resolved URL
// is deliberately not stored: these expire within hours, so a saved one would
// be a queue entry that stops working.
func (a *API) sourcePlay(w http.ResponseWriter, r *http.Request) {
	var c source.Candidate
	if err := decode(r, &c); err != nil || c.ID == "" || c.Provider == "" {
		http.Error(w, "a candidate with id and provider is required", 400)
		return
	}
	location, err := a.sources.Resolve(r.Context(), c)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	title := c.Title
	if title == "" {
		title = "Unknown"
	}
	a.pl.Play([]store.Track{{
		Path:       location,
		Title:      title,
		Artist:     c.Artist,
		DurationMS: c.DurationMS,
	}}, 0)
	writeJSON(w, a.pl.State())
}
