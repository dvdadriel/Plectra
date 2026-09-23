package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/discovery"
)

// Discovery is the slice of the recommender the API is allowed to use.
type Discovery interface {
	Sections(ctx context.Context) ([]discovery.Section, error)
	Home(ctx context.Context) ([]discovery.AlbumSection, error)
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
	mux.HandleFunc("GET /api/home", a.home)
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

// home is the album view: the library and the world's charts in one page of
// sections, each saying why it is there.
func (a *API) home(w http.ResponseWriter, r *http.Request) {
	sections, err := a.discover.Home(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if sections == nil {
		sections = []discovery.AlbumSection{}
	}
	writeJSON(w, sections)
}
