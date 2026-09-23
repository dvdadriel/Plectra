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
// sourcePlay starts a track that is not in the library. The whole album can be
// queued alongside it: entries with no file are left unresolved and looked up
// by the player when their turn comes, which is what makes next and previous
// work on a catalogue album the way they do on a local one.
func (a *API) sourcePlay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		source.Candidate
		// Album is the listing the track was chosen from, in order.
		Album []struct {
			Title   string `json:"title"`
			Artist  string `json:"artist"`
			LocalID int64  `json:"localId"`
		} `json:"album"`
		StartIndex int `json:"startIndex"`
	}
	if err := decode(r, &req); err != nil || req.ID == "" || req.Provider == "" {
		http.Error(w, "a candidate with id and provider is required", 400)
		return
	}

	location, err := a.sources.Resolve(r.Context(), req.Candidate)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	title := req.Title
	if title == "" {
		title = "Unknown"
	}

	// No listing came with it: play the one track, as before.
	if len(req.Album) == 0 {
		a.pl.Play([]store.Track{{
			Path: location, Title: title, Artist: req.Artist,
			DurationMS: req.DurationMS, Ephemeral: true,
		}}, 0)
		writeJSON(w, a.pl.State())
		return
	}

	start := req.StartIndex
	if start < 0 || start >= len(req.Album) {
		start = 0
	}

	// Rows the library already has keep their real file, so they play at once
	// and never cost a lookup.
	local := map[int64]store.Track{}
	var ids []int64
	for _, row := range req.Album {
		if row.LocalID > 0 {
			ids = append(ids, row.LocalID)
		}
	}
	if len(ids) > 0 {
		if ts, err := a.cat.Tracks(r.Context(), ids); err == nil {
			for _, t := range ts {
				local[t.ID] = t
			}
		}
	}

	queue := make([]store.Track, 0, len(req.Album))
	for i, row := range req.Album {
		switch {
		case i == start:
			// The one that was clicked is already resolved.
			queue = append(queue, store.Track{
				Path: location, Title: title, Artist: req.Artist,
				DurationMS: req.DurationMS, Ephemeral: true,
			})
		case row.LocalID > 0:
			if t, ok := local[row.LocalID]; ok {
				queue = append(queue, t)
				continue
			}
			queue = append(queue, store.Track{Title: row.Title, Artist: row.Artist, Ephemeral: true})
		default:
			// No Path: the player looks this up when it reaches it.
			queue = append(queue, store.Track{Title: row.Title, Artist: row.Artist, Ephemeral: true})
		}
	}

	a.pl.Play(queue, start)
	writeJSON(w, a.pl.State())
}
