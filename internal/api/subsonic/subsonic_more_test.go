package subsonic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

// ---------------------------------------------------------------------------
// More of the contract, again restated independently of the production types.
// ---------------------------------------------------------------------------

type jPlaylist struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Comment   string  `json:"comment"`
	Owner     string  `json:"owner"`
	SongCount int     `json:"songCount"`
	Duration  int     `json:"duration"`
	Created   string  `json:"created"`
	Changed   string  `json:"changed"`
	Entry     []jSong `json:"entry"`
}

// jBody2 covers the elements the first test file did not need: the v1 twins and
// the playlist surface. Spelled out by name, so renaming an element fails here.
type jBody2 struct {
	Status string  `json:"status"`
	Error  *jError `json:"error"`

	Playlists *struct {
		Playlist []jPlaylist `json:"playlist"`
	} `json:"playlists"`
	Playlist *jPlaylist `json:"playlist"`

	Indexes *struct {
		Index []jIndex `json:"index"`
	} `json:"indexes"`
	Artists *struct {
		Index []jIndex `json:"index"`
	} `json:"artists"`
	SearchResult2 *struct {
		Song []jSong `json:"song"`
	} `json:"searchResult2"`
	SearchResult3 *struct {
		Song []jSong `json:"song"`
	} `json:"searchResult3"`
	AlbumList *struct {
		Album []jAlbum `json:"album"`
	} `json:"albumList"`
	AlbumList2 *struct {
		Album []jAlbum `json:"album"`
	} `json:"albumList2"`
	Starred *struct {
		Song []jSong `json:"song"`
	} `json:"starred"`
	Starred2 *struct {
		Song []jSong `json:"song"`
	} `json:"starred2"`
}

// call runs a JSON request and decodes it into the wider envelope.
func (h *harness) call(t *testing.T, method string, params url.Values) jBody2 {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("f", "json")
	rec := h.do(t, method, params)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d; every Subsonic reply is 200 with the verdict in the envelope", method, rec.Code)
	}
	var env struct {
		Resp jBody2 `json:"subsonic-response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decoding envelope: %v\nbody: %s", method, err, rec.Body.String())
	}
	return env.Resp
}

// callOK fails unless the call succeeded.
func (h *harness) callOK(t *testing.T, method string, params url.Values) jBody2 {
	t.Helper()
	b := h.call(t, method, params)
	if b.Status != "ok" {
		t.Fatalf("%s: status %q error %+v; want ok", method, b.Status, b.Error)
	}
	return b
}

// ---------------------------------------------------------------------------
// 7. Playlists over the Subsonic surface
// ---------------------------------------------------------------------------

func TestPlaylistLifecycleOverSubsonic(t *testing.T) {
	h := newHarness(t, testPass)
	one, two, three := songID(h.tracks[0].ID), songID(h.tracks[1].ID), songID(h.tracks[2].ID)

	if b := h.callOK(t, "getPlaylists", nil); b.Playlists == nil || len(b.Playlists.Playlist) != 0 {
		t.Fatalf("a fresh library has playlists: %+v", b.Playlists)
	}

	// create, with the songs given up front.
	created := h.callOK(t, "createPlaylist", url.Values{"name": {"Roadtrip"}, "songId": {one, two}})
	if created.Playlist == nil {
		t.Fatal("createPlaylist returned no playlist element")
	}
	pid := created.Playlist.ID
	if created.Playlist.Name != "Roadtrip" || created.Playlist.SongCount != 2 {
		t.Fatalf("createPlaylist returned %+v, want Roadtrip with 2 songs", created.Playlist)
	}

	// The two id spaces clients round-trip: playlist ids are bare numbers,
	// song ids carry the tr- prefix. Mixing them up silently breaks clients.
	if _, err := strconv.ParseInt(pid, 10, 64); err != nil {
		t.Fatalf("playlist id %q is not a bare number; clients round-trip it as playlistId", pid)
	}
	if strings.ContainsAny(pid, "-") {
		t.Fatalf("playlist id %q carries a prefix", pid)
	}
	for _, e := range created.Playlist.Entry {
		if !strings.HasPrefix(e.ID, "tr-") {
			t.Fatalf("playlist entry id %q lacks the tr- prefix", e.ID)
		}
	}

	// getPlaylists lists it, with the same id.
	lists := h.callOK(t, "getPlaylists", nil)
	if lists.Playlists == nil || len(lists.Playlists.Playlist) != 1 {
		t.Fatalf("getPlaylists = %+v, want one playlist", lists.Playlists)
	}
	if got := lists.Playlists.Playlist[0]; got.ID != pid || got.Name != "Roadtrip" || got.SongCount != 2 {
		t.Fatalf("getPlaylists entry = %+v, want id %s Roadtrip songCount 2", got, pid)
	}
	if lists.Playlists.Playlist[0].Owner != serverName {
		t.Fatalf("playlist owner = %q, want %q", lists.Playlists.Playlist[0].Owner, serverName)
	}

	// getPlaylist: entries in the order they were added, with a summed duration.
	got := h.callOK(t, "getPlaylist", url.Values{"id": {pid}})
	if got.Playlist == nil || len(got.Playlist.Entry) != 2 {
		t.Fatalf("getPlaylist = %+v, want 2 entries", got.Playlist)
	}
	if got.Playlist.Entry[0].ID != one || got.Playlist.Entry[1].ID != two {
		t.Fatalf("getPlaylist entries = %s, %s; want %s, %s in insertion order",
			got.Playlist.Entry[0].ID, got.Playlist.Entry[1].ID, one, two)
	}
	if got.Playlist.Entry[0].Title != h.tracks[0].Title {
		t.Fatalf("first entry title = %q, want %q", got.Playlist.Entry[0].Title, h.tracks[0].Title)
	}
	// 143s + 187s, from the stored durations.
	if got.Playlist.Duration != 330 {
		t.Fatalf("playlist duration = %d seconds, want 330", got.Playlist.Duration)
	}

	// updatePlaylist: rename and append in one call.
	h.callOK(t, "updatePlaylist", url.Values{
		"playlistId":  {pid},
		"name":        {"Long Drive"},
		"songIdToAdd": {three},
	})
	after := h.callOK(t, "getPlaylist", url.Values{"id": {pid}})
	if after.Playlist.Name != "Long Drive" {
		t.Fatalf("after rename, name = %q, want Long Drive", after.Playlist.Name)
	}
	if len(after.Playlist.Entry) != 3 || after.Playlist.Entry[2].ID != three {
		t.Fatalf("after songIdToAdd, entries = %+v, want %s appended", after.Playlist.Entry, three)
	}
	if after.Playlist.Duration != 535 {
		t.Fatalf("playlist duration = %d seconds, want 535", after.Playlist.Duration)
	}

	// songIndexToRemove is a POSITION, not a track id. Index 0 must drop the
	// first entry; were it read as an id, track 3 would be the casualty.
	h.callOK(t, "updatePlaylist", url.Values{"playlistId": {pid}, "songIndexToRemove": {"0"}})
	trimmed := h.callOK(t, "getPlaylist", url.Values{"id": {pid}})
	if len(trimmed.Playlist.Entry) != 2 {
		t.Fatalf("after removing index 0, %d entries remain, want 2", len(trimmed.Playlist.Entry))
	}
	if trimmed.Playlist.Entry[0].ID != two || trimmed.Playlist.Entry[1].ID != three {
		t.Fatalf("after removing index 0, entries = %s, %s; want %s, %s",
			trimmed.Playlist.Entry[0].ID, trimmed.Playlist.Entry[1].ID, two, three)
	}

	// An index past the end is ignored rather than erroring or removing the last.
	h.callOK(t, "updatePlaylist", url.Values{"playlistId": {pid}, "songIndexToRemove": {"9"}})
	if still := h.callOK(t, "getPlaylist", url.Values{"id": {pid}}); len(still.Playlist.Entry) != 2 {
		t.Fatalf("index 9 removed something: %+v", still.Playlist.Entry)
	}

	// delete, then the id is gone.
	h.callOK(t, "deletePlaylist", url.Values{"id": {pid}})
	if b := h.callOK(t, "getPlaylists", nil); len(b.Playlists.Playlist) != 0 {
		t.Fatalf("after deletePlaylist, getPlaylists = %+v", b.Playlists.Playlist)
	}
	gone := h.call(t, "getPlaylist", url.Values{"id": {pid}})
	if gone.Status != "failed" || gone.Error == nil || gone.Error.Code != errNotFound {
		t.Fatalf("getPlaylist on a deleted id: status %q error %+v; want failed with code %d",
			gone.Status, gone.Error, errNotFound)
	}

	// createPlaylist without a name is a parameter error, not a nameless list.
	missing := h.call(t, "createPlaylist", nil)
	if missing.Status != "failed" || missing.Error == nil || missing.Error.Code != errParameter {
		t.Fatalf("createPlaylist with no name: status %q error %+v; want code %d",
			missing.Status, missing.Error, errParameter)
	}
}

// ---------------------------------------------------------------------------
// 8. Cover art
// ---------------------------------------------------------------------------

func TestGetCoverArtResolvesAlbumAndSongIDs(t *testing.T) {
	h := newHarness(t, testPass)
	song := h.tracks[0]

	albumRef, _, err := h.st.AlbumCoverState(t.Context(), song.ID)
	if err != nil {
		t.Fatalf("AlbumCoverState: %v", err)
	}

	// Before any artwork is cached: a failed envelope with code 70, not a 500
	// and not an empty 200 that a client would render as a broken image.
	for _, id := range []string{albumID(albumRef), songID(song.ID)} {
		rec := h.do(t, "getCoverArt", url.Values{"id": {id}, "f": {"json"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("getCoverArt %s with no artwork: HTTP %d, want 200 with a failed envelope", id, rec.Code)
		}
		var env struct {
			Resp jBody2 `json:"subsonic-response"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("getCoverArt %s: body is not an envelope: %q", id, rec.Body.String())
		}
		if env.Resp.Status != "failed" || env.Resp.Error == nil || env.Resp.Error.Code != errNotFound {
			t.Fatalf("getCoverArt %s with no artwork: %+v; want failed with code %d", id, env.Resp, errNotFound)
		}
	}

	// Cache artwork for the album and both ids must now serve those bytes. The
	// song path has to resolve to its album: clients ask with whichever id they
	// are holding, and this resolution has been broken before.
	art := []byte("\x89PNG\r\n\x1a\nnot-really-a-png-but-distinct-bytes")
	artPath := filepath.Join(h.dir, "cover.png")
	if err := os.WriteFile(artPath, art, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetAlbumCover(t.Context(), albumRef, artPath); err != nil {
		t.Fatalf("SetAlbumCover: %v", err)
	}

	for _, id := range []string{albumID(albumRef), songID(song.ID)} {
		rec := h.do(t, "getCoverArt", url.Values{"id": {id}})
		if rec.Code != http.StatusOK {
			t.Fatalf("getCoverArt %s: HTTP %d, want 200", id, rec.Code)
		}
		if got := rec.Body.Bytes(); string(got) != string(art) {
			t.Fatalf("getCoverArt %s served %d bytes (%q...), want the cached artwork",
				id, len(got), string(got[:min(len(got), 16)]))
		}
	}

	// A song that does not exist cannot be resolved to an album.
	miss := h.call(t, "getCoverArt", url.Values{"id": {songID(9999)}})
	if miss.Status != "failed" || miss.Error == nil || miss.Error.Code != errNotFound {
		t.Fatalf("getCoverArt for an unknown song: %+v; want failed with code %d", miss, errNotFound)
	}

	// Garbage that is neither id kind is a parameter error, and still an envelope.
	junk := h.call(t, "getCoverArt", url.Values{"id": {"not-an-id"}})
	if junk.Status != "failed" || junk.Error == nil || junk.Error.Code != errParameter {
		t.Fatalf("getCoverArt with a bogus id: %+v; want failed with code %d", junk, errParameter)
	}
}

// ---------------------------------------------------------------------------
// 9. Scrobble
// ---------------------------------------------------------------------------

// fakeScrobbler stands in for the history module through the interface the API
// was given. It records a play only for a submission, which is the contract the
// now-playing flag exists to express.
type fakeScrobbler struct {
	mu    sync.Mutex
	known map[int64]bool
	calls []struct {
		trackID    int64
		submission bool
	}
	plays []int64
	err   error // returned instead of ErrNotFound when set
}

func (f *fakeScrobbler) RecordPlay(_ context.Context, trackID int64, submission bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		trackID    int64
		submission bool
	}{trackID, submission})
	if !f.known[trackID] {
		if f.err != nil {
			return f.err
		}
		return store.ErrNotFound
	}
	if submission {
		f.plays = append(f.plays, trackID)
	}
	return nil
}

func TestScrobbleRecordsSubmissionsOnly(t *testing.T) {
	fake := &fakeScrobbler{known: map[int64]bool{}}
	h := newHarnessWith(t, testPass, fake)
	for _, tr := range h.tracks {
		fake.known[tr.ID] = true
	}
	id := h.tracks[1].ID

	// A submission is a completed listen.
	h.callOK(t, "scrobble", url.Values{"id": {songID(id)}, "submission": {"true"}})
	if len(fake.plays) != 1 || fake.plays[0] != id {
		t.Fatalf("submission=true recorded %v, want one play of %d", fake.plays, id)
	}

	// Now playing is not a listen: the flag must reach the scrobbler as false,
	// and nothing may be counted.
	h.callOK(t, "scrobble", url.Values{"id": {songID(id)}, "submission": {"false"}})
	if len(fake.plays) != 1 {
		t.Fatalf("submission=false counted a play: %v", fake.plays)
	}
	if len(fake.calls) != 2 || fake.calls[1].submission {
		t.Fatalf("submission=false reached the scrobbler as %+v", fake.calls)
	}

	// Subsonic's default is a submission when the parameter is absent.
	h.callOK(t, "scrobble", url.Values{"id": {songID(id)}})
	if len(fake.calls) != 3 || !fake.calls[2].submission {
		t.Fatalf("scrobble without submission= reached the scrobbler as %+v", fake.calls[2])
	}
	if len(fake.plays) != 2 {
		t.Fatalf("default scrobble recorded %v, want a second play", fake.plays)
	}

	// An id that is not a song id never reaches the scrobbler.
	bad := h.call(t, "scrobble", url.Values{"id": {albumID(1)}})
	if bad.Status != "failed" || bad.Error == nil || bad.Error.Code != errParameter {
		t.Fatalf("scrobble with an album id: %+v; want failed with code %d", bad, errParameter)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("scrobble with an album id still called the scrobbler: %+v", fake.calls)
	}

	// A track id that no longer exists: the scrobbler's not-found is tolerated
	// (a client may report a play for something since removed) but a real
	// failure is reported to the client as an error code, never swallowed and
	// never a panic.
	tolerated := h.call(t, "scrobble", url.Values{"id": {songID(4242)}})
	if tolerated.Status != "ok" {
		t.Fatalf("scrobble for a removed track: %+v; a not-found play is tolerated", tolerated)
	}
	fake.err = errBroken
	broke := h.call(t, "scrobble", url.Values{"id": {songID(4242)}})
	if broke.Status != "failed" || broke.Error == nil || broke.Error.Code == 0 && broke.Error.Message == "" {
		t.Fatalf("scrobble when the scrobbler fails: %+v; want a failed envelope with an error", broke)
	}
	if broke.Error.Message != errBroken.Error() {
		t.Fatalf("scrobble error message = %q, want %q", broke.Error.Message, errBroken.Error())
	}

	// A nil Scrobbler is tolerated: no history wired up must not break clients.
	none := newHarness(t, testPass)
	none.callOK(t, "scrobble", url.Values{"id": {songID(none.tracks[0].ID)}, "submission": {"true"}})
}

// errBroken is a scrobbler failure that is not "the track is gone".
var errBroken = errString("history database is down")

type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// 10. The v1 twins and routing
// ---------------------------------------------------------------------------

func TestLegacyMethodsUseTheirOwnElementNames(t *testing.T) {
	h := newHarness(t, testPass)
	h.callOK(t, "star", url.Values{"id": {songID(h.tracks[0].ID)}})

	// Each v1 method must answer under its own element. A client reading
	// <indexes> will see nothing if the server replies with <artists>.
	idx := h.callOK(t, "getIndexes", nil)
	if idx.Indexes == nil || len(idx.Indexes.Index) == 0 {
		t.Fatalf("getIndexes returned no indexes element: %+v", idx)
	}
	if idx.Artists != nil {
		t.Fatal("getIndexes answered under the v2 artists element")
	}

	s2 := h.callOK(t, "search2", url.Values{"query": {"Aurora"}})
	if s2.SearchResult2 == nil || len(s2.SearchResult2.Song) != 1 {
		t.Fatalf("search2 returned %+v, want one song under searchResult2", s2.SearchResult2)
	}
	if s2.SearchResult3 != nil {
		t.Fatal("search2 answered under searchResult3")
	}

	al := h.callOK(t, "getAlbumList", url.Values{"size": {"10"}})
	if al.AlbumList == nil || len(al.AlbumList.Album) != 2 {
		t.Fatalf("getAlbumList returned %+v, want 2 albums under albumList", al.AlbumList)
	}
	if al.AlbumList2 != nil {
		t.Fatal("getAlbumList answered under albumList2")
	}

	st := h.callOK(t, "getStarred", nil)
	if st.Starred == nil || len(st.Starred.Song) != 1 {
		t.Fatalf("getStarred returned %+v, want one song under starred", st.Starred)
	}
	if st.Starred2 != nil {
		t.Fatal("getStarred answered under starred2")
	}

	// Clients call both spellings of every method.
	h.callOK(t, "ping", nil)
	h.callOK(t, "ping.view", nil)
	h.callOK(t, "getIndexes.view", nil)

	// An unknown method is not found, not a 404 and not a panic.
	for _, m := range []string{"getPodcasts", "getPodcasts.view", "totallyMadeUp"} {
		b := h.call(t, m, nil)
		if b.Status != "failed" || b.Error == nil || b.Error.Code != errNotFound {
			t.Fatalf("%s: %+v; want failed with code %d", m, b, errNotFound)
		}
	}
}

// ---------------------------------------------------------------------------
// 11. Range requests on stream
// ---------------------------------------------------------------------------

func TestStreamHonoursRangeRequests(t *testing.T) {
	h := newHarness(t, testPass)
	track := h.tracks[2]
	full, err := os.ReadFile(track.Path)
	if err != nil {
		t.Fatal(err)
	}

	params := url.Values{"u": {testUser}, "p": {h.pass}, "id": {songID(track.ID)}}
	req := httptest.NewRequest(http.MethodGet, "/rest/stream?"+params.Encode(), nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)

	// Range support comes from http.ServeContent; this guards that we keep
	// handing it the file rather than writing the body ourselves. Clients seek
	// inside a track with exactly this request.
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range request: HTTP %d, want 206", rec.Code)
	}
	if got := rec.Body.Len(); got != 100 {
		t.Fatalf("Range bytes=0-99 returned %d bytes, want 100", got)
	}
	if string(rec.Body.Bytes()) != string(full[:100]) {
		t.Fatalf("Range bytes=0-99 returned the wrong 100 bytes")
	}
	wantRange := "bytes 0-99/" + strconv.Itoa(len(full))
	if cr := rec.Header().Get("Content-Range"); cr != wantRange {
		t.Fatalf("Content-Range = %q, want %q", cr, wantRange)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", ar)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type on a partial response = %q, want audio/wav", ct)
	}
}
