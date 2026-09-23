package native

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/plectra/plectra/internal/audio"
	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
)

// fullSink is a fake sink that reports itself full: it consumes no PCM and so
// holds the engine on whatever track it opened. Playback state therefore stays
// wherever a command put it, with no sound card, no clock and no race between
// the test and the end of a fixture file. Frames consumed stay at zero, which
// is the only thing position is ever derived from.
type fullSink struct {
	mu     sync.Mutex
	played int64
}

func (s *fullSink) Write(pcm []float32) (int, error) { return 0, nil }
func (s *fullSink) Format() audio.Format             { return audio.Format{SampleRate: 48000, Channels: 2} }
func (s *fullSink) Played() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.played
}
func (s *fullSink) Close() error { return nil }
func (s *fullSink) Discard()     {}

// writeWAV writes a short 16-bit stereo WAV so audio.Open has something real to
// decode. Generated in code; no binary fixtures in the tree.
func writeWAV(t *testing.T, path string, ms int) {
	t.Helper()
	const rate, ch = 44100, 2
	frames := rate * ms / 1000
	data := make([]byte, frames*ch*2)
	for i := 0; i < frames*ch; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(i%1000*30)))
	}
	var b []byte
	put32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	put16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	b = append(b, "RIFF"...)
	put32(uint32(36 + len(data)))
	b = append(b, "WAVEfmt "...)
	put32(16)
	put16(1)
	put16(ch)
	put32(rate)
	put32(rate * ch * 2)
	put16(ch * 2)
	put16(16)
	b = append(b, "data"...)
	put32(uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type harness struct {
	h      http.Handler
	api    *API // kept so a test can attach an optional dependency and rebuild
	tracks []store.Track
}

// newHarness builds the API over a fixture library of two albums: "First
// Light" (3 tracks) and "Second Wind" (2 tracks).
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "plectra.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fixtures := []store.Track{
		{Title: "Aurora", Artist: "Alpha Band", Album: "First Light", TrackNo: 1, DurationMS: 143000},
		{Title: "Bright", Artist: "Alpha Band", Album: "First Light", TrackNo: 2, DurationMS: 187000},
		{Title: "Cold", Artist: "Alpha Band", Album: "First Light", TrackNo: 3, DurationMS: 205000},
		{Title: "Dusk", Artist: "Beta Choir", Album: "Second Wind", TrackNo: 1, DurationMS: 121000},
		{Title: "Ember", Artist: "Beta Choir", Album: "Second Wind", TrackNo: 2, DurationMS: 164000},
	}
	h := &harness{}
	for i := range fixtures {
		f := fixtures[i]
		f.Path = filepath.Join(dir, strings.ToLower(f.Title)+".wav")
		writeWAV(t, f.Path, 200)
		f.FileHash = f.Title
		f.Format, f.SampleRate, f.Channels = "wav", 44100, 2
		id, err := st.UpsertTrack(t.Context(), f)
		if err != nil {
			t.Fatalf("UpsertTrack %q: %v", f.Title, err)
		}
		f.ID = id
		h.tracks = append(h.tracks, f)
	}

	api := New(catalog.New(st), playlist.New(st), player.New(&fullSink{}), fstest.MapFS{})
	h.api = api
	h.h = api.Handler()
	return h
}

// do drives one request through the mux, which is the only surface a UI has.
func (h *harness) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// state runs a request that must succeed and decodes the player state every
// player endpoint answers with. The player serialises commands and snapshots on
// one goroutine, so the body is the settled state after the command — there is
// nothing to wait for.
func (h *harness) state(t *testing.T, method, path, body string) player.State {
	t.Helper()
	rec := h.do(t, method, path, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: HTTP %d, body %q; want 200", method, path, rec.Code, rec.Body.String())
	}
	var st player.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("%s %s: decoding state: %v\nbody: %s", method, path, err, rec.Body.String())
	}
	return st
}

func titles(q []store.Track) []string {
	out := make([]string, len(q))
	for i, tr := range q {
		out[i] = tr.Title
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEnqueueIntoEmptyQueueStartsPlayback pins the rule that makes enqueue
// worth having: with nothing queued there is nothing to append to, so the
// first append plays rather than leaving the user staring at a silent player.
func TestEnqueueIntoEmptyQueueStartsPlayback(t *testing.T) {
	h := newHarness(t)

	if st := h.state(t, "GET", "/api/player/queue", ""); len(st.Queue) != 0 {
		t.Fatalf("fresh player queue = %v, want empty", titles(st.Queue))
	}

	st := h.state(t, "POST", "/api/player/queue", `{"albumID":2}`)
	if want := []string{"Dusk", "Ember"}; !eq(titles(st.Queue), want) {
		t.Errorf("queue = %v, want %v", titles(st.Queue), want)
	}
	if st.Index != 0 {
		t.Errorf("index = %d, want 0", st.Index)
	}
	// The distinguishing observable: Enqueue would have left this false.
	if !st.Playing {
		t.Error("enqueue into an empty queue did not start playback")
	}
}

// TestEnqueueAppendsWithoutDisturbingPlayback is the other half of the rule:
// once something is playing, an append must not become a "play this instead".
func TestEnqueueAppendsWithoutDisturbingPlayback(t *testing.T) {
	h := newHarness(t)

	st := h.state(t, "POST", "/api/player/play", `{"albumID":1,"startIndex":1}`)
	if st.Index != 1 || len(st.Queue) != 3 {
		t.Fatalf("setup: index %d, queue %v; want index 1 over 3 tracks", st.Index, titles(st.Queue))
	}

	st = h.state(t, "POST", "/api/player/queue", `{"albumID":2}`)
	want := []string{"Aurora", "Bright", "Cold", "Dusk", "Ember"}
	if !eq(titles(st.Queue), want) {
		t.Errorf("queue = %v, want %v (appended, not replaced)", titles(st.Queue), want)
	}
	if st.Index != 1 {
		t.Errorf("index = %d, want 1: appending must not move what is playing", st.Index)
	}
	if !st.Playing {
		t.Error("appending stopped playback")
	}
}

// TestEnqueueRejectsBadRequests covers both 400 paths: a body that is not JSON
// and a body that names nothing the library has.
func TestEnqueueRejectsBadRequests(t *testing.T) {
	h := newHarness(t)

	// The reason is asserted, not just the code: "I could not read that" and
	// "that names no tracks" are different bugs to the client, and a handler
	// that stopped reading the body at all would still answer 400.
	for _, c := range []struct{ name, body, reason string }{
		{"malformed body", `{"albumID":`, "bad body"},
		{"unknown album", `{"albumID":999}`, "nothing to play"},
		{"empty track list", `{"trackIDs":[]}`, "nothing to play"},
	} {
		rec := h.do(t, "POST", "/api/player/queue", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, body %q; want 400", c.name, rec.Code, rec.Body.String())
			continue
		}
		if got := strings.TrimSpace(rec.Body.String()); got != c.reason {
			t.Errorf("%s: 400 reason %q, want %q", c.name, got, c.reason)
		}
	}

	// A rejected request must not have touched the player.
	if st := h.state(t, "GET", "/api/player/queue", ""); len(st.Queue) != 0 || st.Playing {
		t.Errorf("after rejected requests: queue %v, playing %v; want empty and stopped", titles(st.Queue), st.Playing)
	}
}

// TestDequeueRemovesNamedEntry checks the index in the path selects the entry,
// and that a non-numeric index is refused rather than defaulting to zero.
func TestDequeueRemovesNamedEntry(t *testing.T) {
	h := newHarness(t)
	h.state(t, "POST", "/api/player/play", `{"albumID":1}`)

	rec := h.do(t, "DELETE", "/api/player/queue/two", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-numeric index: HTTP %d, body %q; want 400", rec.Code, rec.Body.String())
	}
	if st := h.state(t, "GET", "/api/player/queue", ""); len(st.Queue) != 3 {
		t.Fatalf("a refused delete changed the queue to %v", titles(st.Queue))
	}

	st := h.state(t, "DELETE", "/api/player/queue/1", "")
	if want := []string{"Aurora", "Cold"}; !eq(titles(st.Queue), want) {
		t.Errorf("queue = %v, want %v", titles(st.Queue), want)
	}
}

// The album detail view needs the album's own name, artist and year alongside
// the songs, or it would take two requests to draw one screen.
func TestAlbumEndpointCarriesTheAlbumAndItsTracks(t *testing.T) {
	h := newHarness(t)

	// The fixtures are written before their album rows exist, so ask the API
	// which id "First Light" ended up with.
	var albums []store.Album
	lw := httptest.NewRecorder()
	h.h.ServeHTTP(lw, httptest.NewRequest("GET", "/api/albums", nil))
	if err := json.NewDecoder(lw.Body).Decode(&albums); err != nil {
		t.Fatal(err)
	}
	var albumID int64
	for _, a := range albums {
		if a.Title == "First Light" {
			albumID = a.ID
		}
	}
	if albumID == 0 {
		t.Fatalf("no First Light in %+v", albums)
	}

	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/albums/%d", albumID), nil))
	if w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}

	var got struct {
		ID     int64
		Title  string
		Artist string
		Tracks []store.Track
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "First Light" || got.Artist != "Alpha Band" {
		t.Errorf("album = %q by %q, want First Light by Alpha Band", got.Title, got.Artist)
	}
	if len(got.Tracks) != 3 {
		t.Fatalf("got %d tracks, want the album's 3", len(got.Tracks))
	}
	// Order is what the detail view renders; a shuffled listing would be wrong.
	if got.Tracks[0].Title != "Aurora" || got.Tracks[2].Title != "Cold" {
		t.Errorf("tracks out of order: %q … %q", got.Tracks[0].Title, got.Tracks[2].Title)
	}
}

func TestAlbumEndpointRejectsAnAlbumThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/albums/99999", nil))
	if w.Code != 404 {
		t.Errorf("answered %d for a missing album, want 404", w.Code)
	}
}

func TestAlbumsPageAndReportTheTotal(t *testing.T) {
	h := newHarness(t)

	var first struct {
		Albums []store.Album
		Total  int
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/albums?limit=1&offset=0", nil))
	if err := json.NewDecoder(w.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if len(first.Albums) != 1 || first.Total != 2 {
		t.Fatalf("page = %d albums of %d total; want 1 of 2", len(first.Albums), first.Total)
	}

	var second struct {
		Albums []store.Album
		Total  int
	}
	w = httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/albums?limit=1&offset=1", nil))
	json.NewDecoder(w.Body).Decode(&second)
	if len(second.Albums) != 1 {
		t.Fatalf("second page has %d albums", len(second.Albums))
	}
	if second.Albums[0].Title == first.Albums[0].Title {
		t.Errorf("both pages returned %q; offset was ignored", first.Albums[0].Title)
	}

	// An offset past the end is an empty page, not a 500 and not a wrap-around.
	w = httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/albums?limit=50&offset=999", nil))
	if w.Code != 200 {
		t.Fatalf("offset past the end answered %d", w.Code)
	}
	var past struct{ Albums []store.Album }
	json.NewDecoder(w.Body).Decode(&past)
	if len(past.Albums) != 0 {
		t.Errorf("offset past the end returned %d albums", len(past.Albums))
	}
}

// Without a limit the endpoint answers the plain array it always has, so
// nothing that already calls it has to change.
func TestAlbumsWithoutALimitStillAnswerAPlainList(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET", "/api/albums", nil))

	var al []store.Album
	if err := json.NewDecoder(w.Body).Decode(&al); err != nil {
		t.Fatalf("not a plain array: %v", err)
	}
	if len(al) != 2 {
		t.Errorf("got %d albums, want both", len(al))
	}
}
