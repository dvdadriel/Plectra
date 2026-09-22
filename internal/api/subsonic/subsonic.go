// Package subsonic speaks the OpenSubsonic API, so mature third-party clients
// can drive Plectra without any mobile code being written here.
package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
)

// The version clients negotiate against. 1.16.1 is what current Subsonic and
// OpenSubsonic clients expect.
const (
	apiVersion    = "1.16.1"
	serverName    = "plectra"
	serverVersion = "0.5"
)

// Scrobbler records plays a client reports. Kept as an interface so this package
// does not reach into the history module's internals.
type Scrobbler interface {
	RecordPlay(ctx context.Context, trackID int64, submission bool) error
}

type API struct {
	cat   *catalog.Catalog
	lists *playlist.Service
	pl    *player.Player
	hist  Scrobbler

	user, password string
}

// New builds the Subsonic API. An empty password disables the whole surface:
// this is the one part of Plectra reachable from the network, and it does not
// run without credentials.
func New(cat *catalog.Catalog, lists *playlist.Service, pl *player.Player, hist Scrobbler, user, password string) *API {
	return &API{cat: cat, lists: lists, pl: pl, hist: hist, user: user, password: password}
}

func (a *API) Enabled() bool { return a.password != "" }

// Handler mounts /rest/*. Subsonic clients call both "getPing" and "getPing.view",
// so the suffix is stripped rather than routed twice.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/", func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/"), ".view")
		a.dispatch(w, r, method)
	})
	return mux
}

func (a *API) dispatch(w http.ResponseWriter, r *http.Request, method string) {
	if err := r.ParseForm(); err != nil {
		writeResponse(w, r, errorResponse(errGeneric, "malformed request"))
		return
	}
	if !a.authenticate(r) {
		writeResponse(w, r, errorResponse(errBadAuth, "wrong username or password"))
		return
	}

	ctx := r.Context()
	var (
		res response
		err error
	)
	switch method {
	case "ping":
		res = okResponse()
	case "getLicense":
		res = okResponse()
		res.License = &license{Valid: true}
	case "getMusicFolders":
		res = okResponse()
		res.MusicFolders = &musicFolders{Folder: []musicFolder{{ID: 1, Name: "Music"}}}
	case "getIndexes", "getArtists":
		res, err = a.getArtists(ctx, method)
	case "getArtist":
		res, err = a.getArtist(ctx, r)
	case "getAlbum":
		res, err = a.getAlbum(ctx, r)
	case "getAlbumList2", "getAlbumList":
		res, err = a.getAlbumList(ctx, r, method)
	case "getSong":
		res, err = a.getSong(ctx, r)
	case "search3", "search2":
		res, err = a.search(ctx, r, method)
	case "getPlaylists":
		res, err = a.getPlaylists(ctx)
	case "getPlaylist":
		res, err = a.getPlaylist(ctx, r)
	case "createPlaylist":
		res, err = a.createPlaylist(ctx, r)
	case "updatePlaylist":
		res, err = a.updatePlaylist(ctx, r)
	case "deletePlaylist":
		res, err = a.deletePlaylist(ctx, r)
	case "star", "unstar":
		res, err = a.star(ctx, r, method == "star")
	case "getStarred", "getStarred2":
		res, err = a.getStarred(ctx, method)
	case "scrobble":
		res, err = a.scrobble(ctx, r)
	case "getCoverArt":
		a.getCoverArt(w, r)
		return
	case "stream", "download":
		a.stream(w, r)
		return
	case "jukeboxControl":
		res, err = a.jukebox(ctx, r)
	default:
		writeResponse(w, r, errorResponse(errNotFound, "unknown method "+method))
		return
	}

	if err != nil {
		writeResponse(w, r, errorFor(err))
		return
	}
	writeResponse(w, r, res)
}

// authenticate accepts both the legacy password parameter and the salted token
// scheme; clients in the wild still use either.
func (a *API) authenticate(r *http.Request) bool {
	if !a.Enabled() {
		return false
	}
	if r.Form.Get("u") != a.user {
		return false
	}
	if token, salt := r.Form.Get("t"), r.Form.Get("s"); token != "" && salt != "" {
		sum := md5.Sum([]byte(a.password + salt))
		return subtle.ConstantTimeCompare([]byte(strings.ToLower(token)), []byte(hex.EncodeToString(sum[:]))) == 1
	}
	pw := r.Form.Get("p")
	if enc, ok := strings.CutPrefix(pw, "enc:"); ok {
		if b, err := hex.DecodeString(enc); err == nil {
			pw = string(b)
		}
	}
	return subtle.ConstantTimeCompare([]byte(pw), []byte(a.password)) == 1
}

// ---- ids ----
//
// Subsonic ids are opaque strings shared across entity types, so each carries a
// prefix; a client must never be able to hand an album id to getSong and win.

func artistID(id int64) string { return "ar-" + strconv.FormatInt(id, 10) }
func albumID(id int64) string  { return "al-" + strconv.FormatInt(id, 10) }
func songID(id int64) string   { return "tr-" + strconv.FormatInt(id, 10) }

func parseID(s, prefix string) (int64, bool) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		// Some clients hand back bare numbers; accept those for the expected kind.
		rest = s
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// ---- response plumbing ----

func writeResponse(w http.ResponseWriter, r *http.Request, res response) {
	res.Xmlns = "http://subsonic.org/restapi"
	if r.Form.Get("f") == "json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]response{"subsonic-response": res})
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(res)
}

func okResponse() response {
	return response{
		Status: "ok", Version: apiVersion, Type: serverName,
		ServerVersion: serverVersion, OpenSubsonic: true,
	}
}

// Subsonic error codes used here.
const (
	errGeneric   = 0
	errParameter = 10
	errBadAuth   = 40
	errNotFound  = 70
)

func errorResponse(code int, msg string) response {
	r := okResponse()
	r.Status = "failed"
	r.Error = &apiError{Code: code, Message: msg}
	return r
}

func errorFor(err error) response {
	if err == store.ErrNotFound {
		return errorResponse(errNotFound, "not found")
	}
	return errorResponse(errGeneric, err.Error())
}
