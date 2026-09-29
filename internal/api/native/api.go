// Package native serves the REST + event API for Plectra's own web UI.
// It knows nothing about audio devices or SQL.
package native

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
)

type API struct {
	cat         *catalog.Catalog
	pl          *player.Player
	lists       *playlist.Service
	enrich      Enricher      // nil when no metadata provider is configured
	history     History       // nil when history routes are not wired
	library     Library       // nil when no scanner is wired
	discover    Discovery     // nil when recommendations are off
	radio       Radio         // nil when the station directory is unavailable
	sources     Sources       // nil when no external source is configured
	browser     Browser       // nil when catalogue search is off
	fastBrowser Browser       // nil when the search-as-you-type catalogue is off
	covers      *remoteCovers // nil when no cover directory is configured
	settings    *settings     // nil when the setup routes are off
	web         fs.FS
}

func New(cat *catalog.Catalog, lists *playlist.Service, pl *player.Player, web fs.FS) *API {
	return &API{cat: cat, lists: lists, pl: pl, web: web}
}

// WithCoverCache lets Plectra cache artwork for albums it does not own, which
// the home view's chart rows are made of.
func (a *API) WithCoverCache(dir string) *API {
	if dir != "" {
		a.covers = newRemoteCovers(dir)
	}
	return a
}

// WithMetadata attaches the optional enrichment routes. Metadata is never
// required for playback.
func (a *API) WithMetadata(e Enricher) *API {
	a.enrich = e
	return a
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/albums", a.albums)
	mux.HandleFunc("GET /api/albums/{id}", a.albumTracks)
	mux.HandleFunc("GET /api/artists", a.artists)
	mux.HandleFunc("GET /api/artists/{id}", a.artistTracks)
	mux.HandleFunc("GET /api/tracks/{id}", a.track)
	mux.HandleFunc("GET /api/search", a.search)
	mux.HandleFunc("GET /api/cover/{id}", a.cover)

	mux.HandleFunc("GET /api/playlists", a.playlists)
	mux.HandleFunc("POST /api/playlists", a.createPlaylist)
	mux.HandleFunc("GET /api/playlists/{id}", a.playlistTracks)
	mux.HandleFunc("PATCH /api/playlists/{id}", a.updatePlaylist)
	mux.HandleFunc("DELETE /api/playlists/{id}", a.deletePlaylist)
	mux.HandleFunc("POST /api/playlists/{id}/tracks", a.addToPlaylist)
	mux.HandleFunc("DELETE /api/playlists/{id}/tracks/{trackID}", a.removeFromPlaylist)

	mux.HandleFunc("POST /api/external/adopt", a.adoptExternal)

	mux.HandleFunc("GET /api/likes", a.likes)
	mux.HandleFunc("POST /api/likes/{id}", a.like)
	mux.HandleFunc("DELETE /api/likes/{id}", a.unlike)

	mux.HandleFunc("POST /api/player/play", a.play)
	mux.HandleFunc("POST /api/player/pause", a.simple(a.pl.Pause))
	mux.HandleFunc("POST /api/player/resume", a.simple(a.pl.Resume))
	mux.HandleFunc("POST /api/player/next", a.simple(a.pl.Next))
	mux.HandleFunc("POST /api/player/prev", a.simple(a.pl.Prev))
	mux.HandleFunc("POST /api/player/seek", a.seek)
	mux.HandleFunc("POST /api/player/volume", a.volume)
	mux.HandleFunc("POST /api/player/mode", a.mode)
	mux.HandleFunc("GET /api/player/queue", a.queue)
	mux.HandleFunc("POST /api/player/queue", a.enqueue)
	mux.HandleFunc("DELETE /api/player/queue/{index}", a.dequeue)
	mux.HandleFunc("GET /api/events", a.events)

	a.enrichRoutes(mux)
	a.historyRoutes(mux)
	a.libraryRoutes(mux)
	a.discoverRoutes(mux)
	a.radioRoutes(mux)
	a.sourceRoutes(mux)
	a.browseRoutes(mux)
	a.remoteCoverRoutes(mux)
	a.settingsRoutes(mux)

	mux.Handle("/", http.FileServer(http.FS(a.web)))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// albums answers one page of the library. The whole list is read and sliced
// here rather than in SQL: the query is already sorted and a library that makes
// this expensive would make the scanner expensive first.
func (a *API) albums(w http.ResponseWriter, r *http.Request) {
	al, err := a.cat.Albums(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	total := len(al)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		// No paging asked for: answer the whole list, as this endpoint always has.
		writeJSON(w, al)
		return
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)

	writeJSON(w, struct {
		Albums []store.Album `json:"albums"`
		Total  int           `json:"total"`
	}{al[offset:end], total})
}

func (a *API) artists(w http.ResponseWriter, r *http.Request) {
	ar, err := a.cat.Artists(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ar)
}

func pathID(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }

// albumTracks answers with the album itself as well as its songs: the detail
// view needs a title, an artist and a year, and asking for them separately
// would be two round trips for one screen.
func (a *API) albumTracks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	al, err := a.cat.Album(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	ts, err := a.cat.AlbumTracks(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, struct {
		store.Album
		Tracks []store.Track `json:"tracks"`
	}{al, ts})
}

func (a *API) artistTracks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	ts, err := a.cat.ArtistTracks(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ts)
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := a.cat.Search(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, res)
}

func (a *API) track(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	ts, err := a.cat.Tracks(r.Context(), []int64{id})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(ts) == 0 {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, ts[0])
}

// cover serves cached artwork straight off disk.
func (a *API) cover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	path, err := a.cat.Cover(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

// ---- playlists and likes ----

func (a *API) playlists(w http.ResponseWriter, r *http.Request) {
	ls, err := a.lists.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ls)
}

type playlistBody struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	TrackIDs    []int64 `json:"trackIDs"`
}

func (a *API) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var b playlistBody
	if err := decode(r, &b); err != nil || b.Name == "" {
		http.Error(w, "name is required", 400)
		return
	}
	pl, err := a.lists.Create(r.Context(), b.Name, b.Description)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(b.TrackIDs) > 0 {
		if err := a.lists.Add(r.Context(), pl.ID, b.TrackIDs); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	writeJSON(w, pl)
}

func (a *API) playlistTracks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	ts, err := a.lists.Tracks(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ts)
}

func (a *API) updatePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var b playlistBody
	if err := decode(r, &b); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	// A body carrying trackIDs is a reorder; one carrying a name is a rename.
	if b.TrackIDs != nil {
		if err := a.lists.Reorder(r.Context(), id, b.TrackIDs); err != nil {
			status(w, err)
			return
		}
	}
	if b.Name != "" {
		if err := a.lists.Update(r.Context(), id, b.Name, b.Description); err != nil {
			status(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if err := a.lists.Delete(r.Context(), id); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) addToPlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	var b playlistBody
	if err := decode(r, &b); err != nil || len(b.TrackIDs) == 0 {
		http.Error(w, "trackIDs is required", 400)
		return
	}
	if err := a.lists.Add(r.Context(), id, b.TrackIDs); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) removeFromPlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	trackID, terr := strconv.ParseInt(r.PathValue("trackID"), 10, 64)
	if err != nil || terr != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if err := a.lists.Remove(r.Context(), id, trackID); err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adoptExternal gives a track from a catalogue a row in the library so the rest
// of the product — likes, playlists, the queue — can refer to it by id like
// anything else. It has no file; the player resolves a stream when it plays.
// Adopting a track twice returns the row it already has.
func (a *API) adoptExternal(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Title      string `json:"title"`
		Artist     string `json:"artist"`
		Album      string `json:"album"`
		DurationMS int64  `json:"durationMs"`
	}
	if err := decode(r, &b); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	t, err := a.cat.Adopt(r.Context(), store.Track{
		Title: b.Title, Artist: b.Artist, Album: b.Album, DurationMS: b.DurationMS,
	})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, t)
}

func (a *API) likes(w http.ResponseWriter, r *http.Request) {
	ts, err := a.lists.Liked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, ts)
}

func (a *API) like(w http.ResponseWriter, r *http.Request)   { a.setLike(w, r, true) }
func (a *API) unlike(w http.ResponseWriter, r *http.Request) { a.setLike(w, r, false) }

func (a *API) setLike(w http.ResponseWriter, r *http.Request, on bool) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	if on {
		err = a.lists.Like(r.Context(), id)
	} else {
		err = a.lists.Unlike(r.Context(), id)
	}
	if err != nil {
		status(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func status(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", 404)
		return
	}
	http.Error(w, err.Error(), 500)
}

func (a *API) simple(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn()
		writeJSON(w, a.pl.State())
	}
}

type playReq struct {
	TrackIDs   []int64 `json:"trackIDs"`
	AlbumID    int64   `json:"albumID"`
	ArtistID   int64   `json:"artistID"`
	PlaylistID int64   `json:"playlistID"`
	StartIndex int     `json:"startIndex"`
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// tracksFor turns a playReq into the tracks it names, whatever shape it came
// in as. Play and enqueue take the same body, so they read it the same way.
func (a *API) tracksFor(r *http.Request, req playReq) ([]store.Track, error) {
	switch {
	case req.AlbumID > 0:
		return a.cat.AlbumTracks(r.Context(), req.AlbumID)
	case req.ArtistID > 0:
		return a.cat.ArtistTracks(r.Context(), req.ArtistID)
	case req.PlaylistID > 0:
		return a.lists.Tracks(r.Context(), req.PlaylistID)
	default:
		return a.cat.Tracks(r.Context(), req.TrackIDs)
	}
}

// readTracks decodes and resolves in one step, reporting to the client itself.
// It returns ok=false once anything has been written to w.
func (a *API) readTracks(w http.ResponseWriter, r *http.Request) (playReq, []store.Track, bool) {
	var req playReq
	if err := decode(r, &req); err != nil {
		http.Error(w, "bad body", 400)
		return req, nil, false
	}
	tracks, err := a.tracksFor(r, req)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return req, nil, false
	}
	if len(tracks) == 0 {
		http.Error(w, "nothing to play", 400)
		return req, nil, false
	}
	return req, tracks, true
}

func (a *API) play(w http.ResponseWriter, r *http.Request) {
	req, tracks, ok := a.readTracks(w, r)
	if !ok {
		return
	}
	a.pl.Play(tracks, req.StartIndex)
	writeJSON(w, a.pl.State())
}

// enqueue appends to the queue instead of replacing it. An empty queue means
// nothing is playing, so the first append starts playback rather than sitting
// silent and making the user press play.
func (a *API) enqueue(w http.ResponseWriter, r *http.Request) {
	_, tracks, ok := a.readTracks(w, r)
	if !ok {
		return
	}
	if len(a.pl.State().Queue) == 0 {
		a.pl.Play(tracks, 0)
	} else {
		a.pl.Enqueue(tracks)
	}
	writeJSON(w, a.pl.State())
}

func (a *API) dequeue(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "bad index", 400)
		return
	}
	a.pl.Remove(i)
	writeJSON(w, a.pl.State())
}

func (a *API) seek(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PositionMS int64 `json:"positionMs"`
	}
	if err := decode(r, &req); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	a.pl.SeekMS(req.PositionMS)
	writeJSON(w, a.pl.State())
}

func (a *API) volume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level float64 `json:"level"`
	}
	if err := decode(r, &req); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	a.pl.SetVolume(req.Level)
	writeJSON(w, a.pl.State())
}

func (a *API) mode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Shuffle bool   `json:"shuffle"`
		Repeat  string `json:"repeat"`
	}
	if err := decode(r, &req); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	a.pl.SetMode(req.Shuffle, req.Repeat)
	writeJSON(w, a.pl.State())
}

func (a *API) queue(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.pl.State()) }

// events streams player state to the UI.
// ponytail: Server-Sent Events, not WebSocket — the spec only needs server->client
// broadcast, and SSE is stdlib. Swap in a WS library if the UI ever needs to talk back.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch, cancel := a.pl.Subscribe()
	defer cancel()

	enc := json.NewEncoder(w)
	send := func(v any) {
		w.Write([]byte("data: "))
		enc.Encode(v)
		w.Write([]byte("\n"))
		flusher.Flush()
	}
	st := a.pl.State()
	send(player.Event{Type: "state", State: &st})

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			send(ev)
		}
	}
}
