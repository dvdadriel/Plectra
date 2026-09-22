package native

import (
	"context"
	"net/http"
	"strings"
)

// Credentials is the slice of credential handling the API is allowed to use.
// Values go in; they never come back out.
type Credentials interface {
	// Status reports which credentials are set, and anything worth telling the
	// user about them — never the values themselves.
	Status(ctx context.Context) map[string]any
	// Save stores the credentials that are present in the request and applies
	// them immediately. A field explicitly set to "" clears that credential.
	Save(ctx context.Context, values map[string]string) error
}

func (a *API) WithCredentials(c Credentials) *API {
	a.creds = c
	return a
}

func (a *API) credentialRoutes(mux *http.ServeMux) {
	if a.creds == nil {
		return
	}
	mux.HandleFunc("GET /api/settings/credentials", a.credentialStatus)
	mux.HandleFunc("POST /api/settings/credentials", a.saveCredentials)
}

func (a *API) credentialStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.creds.Status(r.Context()))
}

// credentialFields is the whole accepted set: anything else in the body is
// ignored rather than stored.
var credentialFields = []string{
	"spotifyClientId", "spotifyClientSecret", "lastfmApiKey", "listenbrainzToken",
}

func (a *API) saveCredentials(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := decode(r, &body); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	values := map[string]string{}
	for _, f := range credentialFields {
		if v, ok := body[f]; ok {
			values[f] = strings.TrimSpace(v)
		}
	}
	if len(values) == 0 {
		http.Error(w, "nothing to save", 400)
		return
	}
	if err := a.creds.Save(r.Context(), values); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, a.creds.Status(r.Context()))
}
