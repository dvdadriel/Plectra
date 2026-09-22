package subsonic

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/plectra/plectra/internal/store"
)

// stream sends the file itself to the client's device. This is the other half of
// the pair with jukeboxControl, which instead drives the speaker on this machine;
// the client chooses which it wants.
//
// No transcoding: the file goes out as it is on disk. Clients that cannot play a
// format say so, and that is a smaller problem than a transcoder nobody asked for.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.Form.Get("id"), "tr-")
	if !ok {
		writeResponse(w, r, errorResponse(errParameter, "id is required"))
		return
	}
	tracks, err := a.cat.Tracks(r.Context(), []int64{id})
	if err != nil || len(tracks) == 0 {
		writeResponse(w, r, errorResponse(errNotFound, "not found"))
		return
	}
	f, err := os.Open(tracks[0].Path)
	if err != nil {
		writeResponse(w, r, errorResponse(errNotFound, "file is gone"))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeResponse(w, r, errorResponse(errGeneric, err.Error()))
		return
	}
	// ServeContent gives range requests and seeking for free, which clients need.
	w.Header().Set("Content-Type", contentType(suffixOf(tracks[0].Path)))
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func suffixOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return lower(path[i+1:])
		}
	}
	return ""
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (a *API) getCoverArt(w http.ResponseWriter, r *http.Request) {
	raw := r.Form.Get("id")

	// Clients pass either an album id or a song id here; a song's cover art is
	// its album's, so the song case is resolved to an album first.
	var id int64
	if strings.HasPrefix(raw, "tr-") {
		songValue, ok := parseID(raw, "tr-")
		if !ok {
			writeResponse(w, r, errorResponse(errParameter, "bad song id"))
			return
		}
		tracks, err := a.cat.Tracks(r.Context(), []int64{songValue})
		if err != nil || len(tracks) == 0 {
			writeResponse(w, r, errorResponse(errNotFound, "not found"))
			return
		}
		id = tracks[0].AlbumID
	} else {
		albumValue, ok := parseID(raw, "al-")
		if !ok {
			writeResponse(w, r, errorResponse(errParameter, "id is required"))
			return
		}
		id = albumValue
	}

	path, err := a.cat.Cover(r.Context(), id)
	if err != nil {
		writeResponse(w, r, errorResponse(errNotFound, "no cover art"))
		return
	}
	http.ServeFile(w, r, path)
}

// scrobble records a play the client reports. Plectra stores it exactly like a
// local play, so statistics do not depend on which device did the listening.
func (a *API) scrobble(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "tr-")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	if a.hist == nil {
		return okResponse(), nil
	}
	// submission=false means "now playing", which is not a completed listen.
	submission := r.Form.Get("submission") != "false"
	if err := a.hist.RecordPlay(ctx, id, submission); err != nil && err != store.ErrNotFound {
		return response{}, err
	}
	return okResponse(), nil
}
