package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/discovery"
)

// Discovery is the slice of the recommender the API is allowed to use.
type Discovery interface {
	Sections(ctx context.Context) ([]discovery.Section, error)
}

func (a *API) WithDiscovery(d Discovery) *API {
	a.discover = d
	return a
}

func (a *API) discoverRoutes(mux *http.ServeMux) {
	if a.discover == nil {
		return
	}
	mux.HandleFunc("GET /api/discover", a.discoverSections)
}

func (a *API) discoverSections(w http.ResponseWriter, r *http.Request) {
	sections, err := a.discover.Sections(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if sections == nil {
		sections = []discovery.Section{}
	}
	writeJSON(w, sections)
}
