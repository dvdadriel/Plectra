package native

import (
	"context"
	"net/http"
	"strconv"

	"github.com/plectra/plectra/internal/radio"
	"github.com/plectra/plectra/internal/store"
)

// Radio is the slice of the station directory the API is allowed to use.
type Radio interface {
	Top(ctx context.Context, limit, offset int) ([]radio.Station, bool, error)
	Search(ctx context.Context, query string, limit, offset int) ([]radio.Station, bool, error)
	Click(ctx context.Context, uuid string)
}

func (a *API) WithRadio(r Radio) *API {
	a.radio = r
	return a
}

func (a *API) radioRoutes(mux *http.ServeMux) {
	if a.radio == nil {
		return
	}
	mux.HandleFunc("GET /api/radio", a.radioStations)
	mux.HandleFunc("POST /api/radio/play", a.radioPlay)
}

// radioStations answers one page. `more` comes from the directory rather than
// from the length of the list: unplayable codecs are filtered out, so a short
// page does not mean the last page.
func (a *API) radioStations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	var (
		stations []radio.Station
		more     bool
		err      error
	)
	if q := r.URL.Query().Get("q"); q != "" {
		stations, more, err = a.radio.Search(r.Context(), q, limit, offset)
	} else {
		stations, more, err = a.radio.Top(r.Context(), limit, offset)
	}
	if err != nil {
		http.Error(w, err.Error(), 502) // the directory is someone else's server
		return
	}
	if stations == nil {
		stations = []radio.Station{}
	}
	writeJSON(w, struct {
		Stations []radio.Station `json:"stations"`
		More     bool            `json:"more"`
	}{stations, more})
}

// radioPlay hands the player a station as a one-entry queue. The station is not
// a library track: it has no id, so nothing is scrobbled and no play is
// recorded against a file that does not exist.
func (a *API) radioPlay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		UUID string `json:"uuid"`
	}
	if err := decode(r, &req); err != nil || req.URL == "" {
		http.Error(w, "url is required", 400)
		return
	}
	a.pl.Play([]store.Track{{
		Path:   req.URL,
		Title:  req.Name,
		Artist: "Radio",
	}}, 0)

	// Registering the play is how the directory's rankings stay honest.
	go a.radio.Click(context.WithoutCancel(r.Context()), req.UUID)

	writeJSON(w, a.pl.State())
}
