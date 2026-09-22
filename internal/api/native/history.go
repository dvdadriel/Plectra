package native

import (
	"context"
	"net/http"

	"github.com/plectra/plectra/internal/history"
	"github.com/plectra/plectra/internal/store"
)

// History is the slice of the recorder the API is allowed to use.
type History interface {
	ImportGDPR(ctx context.Context, path string) (history.ImportResult, error)
	Rematch(ctx context.Context) (int, error)
	Stats(ctx context.Context) (store.HistoryStats, error)
}

// WithHistory attaches the listening-history routes.
func (a *API) WithHistory(h History) *API {
	a.history = h
	return a
}

func (a *API) historyRoutes(mux *http.ServeMux) {
	if a.history == nil {
		return
	}
	mux.HandleFunc("GET /api/history", a.historyStats)
	mux.HandleFunc("POST /api/history/import", a.historyImport)
	mux.HandleFunc("POST /api/history/rematch", a.historyRematch)
}

func (a *API) historyStats(w http.ResponseWriter, r *http.Request) {
	st, err := a.history.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, st)
}

// historyImport reads a Spotify privacy export from a path on this machine.
// The export arrives as a zip the user unpacks themselves; asking for a path
// avoids an upload endpoint for a file that is already local.
func (a *API) historyImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil || req.Path == "" {
		http.Error(w, "path is required", 400)
		return
	}
	res, err := a.history.ImportGDPR(r.Context(), req.Path)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, res)
}

func (a *API) historyRematch(w http.ResponseWriter, r *http.Request) {
	n, err := a.history.Rematch(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"claimed": n})
}
