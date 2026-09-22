package native

import "net/http"

// importPlaylists reads an export file produced in the user's own browser and
// turns it into local playlists.
func (a *API) importPlaylists(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil || req.Path == "" {
		http.Error(w, "path is required", 400)
		return
	}
	res, err := a.lists.ImportFile(r.Context(), req.Path)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, res)
}
