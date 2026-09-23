package native

import (
	"encoding/json"
	"net/http"

	"github.com/plectra/plectra/internal/playlist"
)

// spotifyOrigin is where the connect script runs. The import endpoint answers
// cross-origin requests from there and nowhere else: the browser holds the
// Spotify session, so the export is assembled there and posted here.
const spotifyOrigin = "https://open.spotify.com"

func allowConnect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == spotifyOrigin {
		w.Header().Set("Access-Control-Allow-Origin", spotifyOrigin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	}
}

// importPlaylists takes either an export posted by the connect script or the
// path of one saved to disk.
func (a *API) importPlaylists(w http.ResponseWriter, r *http.Request) {
	allowConnect(w, r)

	var req struct {
		playlist.Export
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "an export or a path is required", 400)
		return
	}

	var (
		res playlist.ImportResult
		err error
	)
	if req.Path != "" {
		res, err = a.lists.ImportFile(r.Context(), req.Path)
	} else {
		res, err = a.lists.Import(r.Context(), req.Export)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, res)
}

// importPreflight answers the browser's CORS check before the POST.
func (a *API) importPreflight(w http.ResponseWriter, r *http.Request) {
	allowConnect(w, r)
	w.WriteHeader(http.StatusNoContent)
}
