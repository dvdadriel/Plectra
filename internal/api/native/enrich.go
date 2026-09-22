package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/store"
)

// Enricher is the slice of the metadata worker the API is allowed to use.
type Enricher interface {
	Seed(ctx context.Context) (int, error)
	Reset(ctx context.Context, provider string) error
	Stats(ctx context.Context) (store.JobStats, error)
}

func (a *API) enrichRoutes(mux *http.ServeMux) {
	if a.enrich == nil {
		return
	}
	mux.HandleFunc("GET /api/enrich", a.enrichStats)
	mux.HandleFunc("POST /api/enrich", a.enrichRun)
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
