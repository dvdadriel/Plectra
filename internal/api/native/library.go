package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/library"
)

// Library is the slice of the scanner the API is allowed to use. Scanning is a
// deliberate action now: the user asks for it, nothing starts it on their behalf.
type Library interface {
	StartScan(ctx context.Context) bool
	Status() library.Status
}

// WithLibrary attaches the scan routes.
func (a *API) WithLibrary(l Library) *API {
	a.library = l
	return a
}

func (a *API) libraryRoutes(mux *http.ServeMux) {
	if a.library == nil {
		return
	}
	mux.HandleFunc("GET /api/library/scan", a.scanStatus)
	mux.HandleFunc("POST /api/library/scan", a.startScan)
}

func (a *API) scanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.library.Status())
}

func (a *API) startScan(w http.ResponseWriter, r *http.Request) {
	// The scan outlives this request, so it must not be cancelled when the
	// browser gets its answer.
	started := a.library.StartScan(context.WithoutCancel(r.Context()))
	st := a.library.Status()
	if !started {
		// Already running is not an error: the user pressed the button twice.
		w.WriteHeader(http.StatusAccepted)
	}
	writeJSON(w, st)
}
