package subsonic

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/audio"
	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
)

// ---------------------------------------------------------------------------
// The contract, restated independently of the production types.
//
// These structs are deliberately NOT the package's own response structs: a
// contract test has to fail when a field is renamed or moved, so it spells out
// the element and attribute names OpenSubsonic clients actually read.
// ---------------------------------------------------------------------------

type jError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jArtist struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AlbumCount int    `json:"albumCount"`
}

type jAlbum struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	ArtistID  string `json:"artistId"`
	SongCount int    `json:"songCount"`
	Duration  int    `json:"duration"`
	Year      int    `json:"year"`
}

type jSong struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Album       string `json:"album"`
	Artist      string `json:"artist"`
	Track       int    `json:"track"`
	Duration    int    `json:"duration"`
	AlbumID     string `json:"albumId"`
	ArtistID    string `json:"artistId"`
	ContentType string `json:"contentType"`
	Suffix      string `json:"suffix"`
}

type jIndex struct {
	Name   string    `json:"name"`
	Artist []jArtist `json:"artist"`
}

type jStatus struct {
	CurrentIndex int     `json:"currentIndex"`
	Playing      bool    `json:"playing"`
	Gain         float64 `json:"gain"`
	Position     int     `json:"position"`
}

type jBody struct {
	Status        string  `json:"status"`
	Version       string  `json:"version"`
	Type          string  `json:"type"`
	ServerVersion string  `json:"serverVersion"`
	Error         *jError `json:"error"`

	Artists *struct {
		Index []jIndex `json:"index"`
	} `json:"artists"`
	Artist *struct {
		jArtist
		Album []jAlbum `json:"album"`
	} `json:"artist"`
	Album *struct {
		jAlbum
		Song []jSong `json:"song"`
	} `json:"album"`
	AlbumList2 *struct {
		Album []jAlbum `json:"album"`
	} `json:"albumList2"`
	Song          *jSong `json:"song"`
	SearchResult3 *struct {
		Artist []jArtist `json:"artist"`
		Album  []jAlbum  `json:"album"`
		Song   []jSong   `json:"song"`
	} `json:"searchResult3"`
	Starred2 *struct {
		Song []jSong `json:"song"`
	} `json:"starred2"`
	JukeboxStatus   *jStatus `json:"jukeboxStatus"`
	JukeboxPlaylist *struct {
		jStatus
		Entry []jSong `json:"entry"`
	} `json:"jukeboxPlaylist"`
}

type jEnvelope struct {
	Resp jBody `json:"subsonic-response"`
}

// ---------------------------------------------------------------------------
// Harness: real store, real catalog/playlist services, real player on a fake sink.
// ---------------------------------------------------------------------------

// silentSink consumes PCM as fast as it is offered. No sound card, and playback
// position still comes from frames it reports as consumed.
type silentSink struct{ played int64 }

func (s *silentSink) Write(pcm []float32) (int, error) {
	s.played += int64(len(pcm) / 2)
	return len(pcm), nil
}
func (s *silentSink) Format() audio.Format { return audio.Format{SampleRate: 48000, Channels: 2} }
func (s *silentSink) Played() int64        { return s.played }
func (s *silentSink) Close() error         { return nil }

const (
	testUser = "plectra"
	testPass = "hunter2"
)

type harness struct {
	h      http.Handler
	dir    string
	tracks []store.Track // as stored, in insertion order
	pass   string
}

// writeWAV lays down a real, decodable 16-bit stereo WAV so stream and the
// player have actual bytes to work with. Fixtures are generated, never committed.
func writeWAV(t *testing.T, path string, ms int) []byte {
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
	return b
}

// newHarness builds the API over a fixture library of 2 artists, 2 albums and
// 3 tracks. An empty password leaves the API disabled, which is a case under test.
func newHarness(t *testing.T, password string) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "plectra.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fixtures := []store.Track{
		{Title: "Aurora Borealis", Artist: "Alpha Band", Album: "First Light", Year: 2001, TrackNo: 1, DiscNo: 1, DurationMS: 143000},
		{Title: "Bright Morning", Artist: "Alpha Band", Album: "First Light", Year: 2001, TrackNo: 2, DiscNo: 1, DurationMS: 187000},
		{Title: "Cold Evening", Artist: "Beta Choir", Album: "Second Wind", Year: 2011, TrackNo: 1, DiscNo: 1, DurationMS: 205000},
	}
	h := &harness{dir: dir, pass: password}
	for i := range fixtures {
		f := fixtures[i]
		f.Path = filepath.Join(dir, strings.ReplaceAll(f.Title, " ", "_")+".wav")
		// The file on disk is short; the stored duration is what the API reports.
		writeWAV(t, f.Path, 300)
		f.FileHash = f.Title
		f.Format, f.SampleRate, f.Channels = "wav", 44100, 2
		id, err := st.UpsertTrack(t.Context(), f)
		if err != nil {
			t.Fatalf("UpsertTrack %q: %v", f.Title, err)
		}
		f.ID = id
		h.tracks = append(h.tracks, f)
	}

	api := New(catalog.New(st), playlist.New(st), player.New(&silentSink{}), nil, testUser, password)
	h.h = api.Handler()
	return h
}

// do drives a request through the HTTP surface, which is the only contract.
func (h *harness) do(t *testing.T, method string, params url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	if params.Get("u") == "" {
		params.Set("u", testUser)
	}
	if params.Get("p") == "" && params.Get("t") == "" {
		params.Set("p", h.pass)
	}
	req := httptest.NewRequest(http.MethodGet, "/rest/"+method+"?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// json2 runs a call asking for JSON and decodes the envelope.
func (h *harness) json2(t *testing.T, method string, params url.Values) jBody {
	t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("f", "json")
	rec := h.do(t, method, params)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d, Subsonic reports failure in the envelope and must still answer 200", method, rec.Code)
	}
	var env jEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decoding JSON envelope: %v\nbody: %s", method, err, rec.Body.String())
	}
	return env.Resp
}

// ok fails unless the call succeeded.
func (h *harness) ok(t *testing.T, method string, params url.Values) jBody {
	t.Helper()
	b := h.json2(t, method, params)
	if b.Status != "ok" {
		t.Fatalf("%s: status %q, error %+v; want ok", method, b.Status, b.Error)
	}
	return b
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// 1. Auth
// ---------------------------------------------------------------------------

func TestAuthContract(t *testing.T) {
	// No password configured: the whole surface is off, whatever is presented.
	// The empty password matters as much as a wrong one — an empty configured
	// secret must not become a secret anyone can guess.
	off := newHarness(t, "")
	emptySalt := "abc"
	emptySum := md5.Sum([]byte("" + emptySalt))
	for _, m := range []string{"ping", "getArtists", "getAlbumList2"} {
		for _, creds := range []url.Values{
			{"p": {"anything"}},
			{"p": {""}},
			{"p": {"enc:"}},
			{"t": {hex.EncodeToString(emptySum[:])}, "s": {emptySalt}},
		} {
			b := off.json2(t, m, cloneWithJSON(creds))
			if b.Status != "failed" || b.Error == nil || b.Error.Code != errBadAuth {
				t.Fatalf("%s with no password configured and creds %v: status %q error %+v; want failed/40",
					m, creds, b.Status, b.Error)
			}
		}
	}

	h := newHarness(t, testPass)

	// (a) plain password
	if b := h.ok(t, "ping", url.Values{"p": {testPass}}); b.Version != apiVersion {
		t.Fatalf("plain password ping: version %q", b.Version)
	}
	// (b) hex-encoded password
	h.ok(t, "ping", url.Values{"p": {"enc:" + hex.EncodeToString([]byte(testPass))}})
	// (c) salted token
	salt := "c19b2d"
	sum := md5.Sum([]byte(testPass + salt))
	h.ok(t, "ping", url.Values{"t": {hex.EncodeToString(sum[:])}, "s": {salt}})

	// Wrong credentials fail in the envelope, not the status line.
	bad := []struct {
		name   string
		params url.Values
	}{
		{"wrong password", url.Values{"p": {"nope"}}},
		{"wrong token", url.Values{"t": {strings.Repeat("0", 32)}, "s": {salt}}},
		{"right token, wrong salt", url.Values{"t": {hex.EncodeToString(sum[:])}, "s": {"other"}}},
		{"unknown user", url.Values{"u": {"mallory"}, "p": {testPass}}},
	}
	for _, c := range bad {
		rec := h.do(t, "ping", cloneWithJSON(c.params))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d; Subsonic errors are still HTTP 200", c.name, rec.Code)
		}
		var env jEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if env.Resp.Status != "failed" || env.Resp.Error == nil || env.Resp.Error.Code != errBadAuth {
			t.Fatalf("%s: status %q error %+v; want failed with code 40", c.name, env.Resp.Status, env.Resp.Error)
		}
	}
}

func cloneWithJSON(v url.Values) url.Values {
	out := url.Values{"f": {"json"}}
	for k, vs := range v {
		out[k] = vs
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. Envelope
// ---------------------------------------------------------------------------

func TestEnvelopeJSONAndXML(t *testing.T) {
	h := newHarness(t, testPass)

	b := h.ok(t, "ping", nil)
	if b.Version != apiVersion || b.Type != serverName || b.ServerVersion != serverVersion {
		t.Fatalf("json envelope: %+v; want version %s type %s serverVersion %s",
			b, apiVersion, serverName, serverVersion)
	}
	// The payload must live under the "subsonic-response" key, nowhere else.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(h.do(t, "ping", url.Values{"f": {"json"}}).Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("json body has keys %v; want exactly subsonic-response", keysOf(raw))
	}
	if _, ok := raw["subsonic-response"]; !ok {
		t.Fatalf("json body has keys %v; want subsonic-response", keysOf(raw))
	}

	// Default format is XML, in the Subsonic namespace.
	rec := h.do(t, "ping", nil)
	var x struct {
		XMLName xml.Name
		Status  string `xml:"status,attr"`
		Version string `xml:"version,attr"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &x); err != nil {
		t.Fatalf("decoding XML: %v\nbody: %s", err, rec.Body.String())
	}
	if x.XMLName.Local != "subsonic-response" {
		t.Fatalf("XML root is %q, want subsonic-response", x.XMLName.Local)
	}
	if x.XMLName.Space != "http://subsonic.org/restapi" {
		t.Fatalf("XML root namespace is %q, want http://subsonic.org/restapi", x.XMLName.Space)
	}
	if x.Status != "ok" || x.Version != apiVersion {
		t.Fatalf("XML envelope status=%q version=%q", x.Status, x.Version)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Fatalf("XML reply Content-Type %q", ct)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// 3. Id namespacing
// ---------------------------------------------------------------------------

func TestIDsAreNamespacedPerEntity(t *testing.T) {
	h := newHarness(t, testPass)
	song := h.tracks[0]

	arts := h.ok(t, "getArtists", nil)
	if arts.Artists == nil || len(arts.Artists.Index) == 0 {
		t.Fatal("getArtists returned no index")
	}
	var artistRef string
	for _, idx := range arts.Artists.Index {
		for _, a := range idx.Artist {
			if !strings.HasPrefix(a.ID, "ar-") {
				t.Fatalf("getArtists: artist id %q lacks the ar- prefix", a.ID)
			}
			if a.Name == "Alpha Band" {
				artistRef = a.ID
			}
		}
	}
	if artistRef == "" {
		t.Fatal("getArtists did not list Alpha Band")
	}

	ar := h.ok(t, "getArtist", url.Values{"id": {artistRef}})
	if ar.Artist == nil || ar.Artist.ID != artistRef {
		t.Fatalf("getArtist returned %+v, want id %s", ar.Artist, artistRef)
	}
	for _, al := range ar.Artist.Album {
		if !strings.HasPrefix(al.ID, "al-") || al.ArtistID != artistRef {
			t.Fatalf("getArtist album %+v: want al- id and artistId %s", al, artistRef)
		}
	}

	al := h.ok(t, "getAlbum", url.Values{"id": {ar.Artist.Album[0].ID}})
	for _, s := range al.Album.Song {
		if !strings.HasPrefix(s.ID, "tr-") || !strings.HasPrefix(s.AlbumID, "al-") || !strings.HasPrefix(s.ArtistID, "ar-") {
			t.Fatalf("getAlbum song %+v: ids not namespaced", s)
		}
	}

	sg := h.ok(t, "getSong", url.Values{"id": {songID(song.ID)}})
	if sg.Song == nil || sg.Song.ID != songID(song.ID) || sg.Song.Title != song.Title {
		t.Fatalf("getSong returned %+v, want %s/%s", sg.Song, songID(song.ID), song.Title)
	}

	sr := h.ok(t, "search3", url.Values{"query": {"Aurora"}})
	if sr.SearchResult3 == nil || len(sr.SearchResult3.Song) == 0 {
		t.Fatal("search3 found nothing for Aurora")
	}
	for _, s := range sr.SearchResult3.Song {
		if !strings.HasPrefix(s.ID, "tr-") {
			t.Fatalf("search3 song id %q lacks the tr- prefix", s.ID)
		}
	}
	for _, a := range sr.SearchResult3.Artist {
		if !strings.HasPrefix(a.ID, "ar-") {
			t.Fatalf("search3 artist id %q lacks the ar- prefix", a.ID)
		}
	}
	for _, a := range sr.SearchResult3.Album {
		if !strings.HasPrefix(a.ID, "al-") {
			t.Fatalf("search3 album id %q lacks the al- prefix", a.ID)
		}
	}

	// An id of the wrong kind must be refused, never silently resolved against
	// whatever row happens to share that number.
	crossed := []struct{ method, id string }{
		{"getSong", albumID(1)},
		{"getSong", artistID(1)},
		{"getArtist", songID(song.ID)},
		{"getArtist", albumID(1)},
		{"getAlbum", songID(song.ID)},
	}
	for _, c := range crossed {
		b := h.json2(t, c.method, url.Values{"id": {c.id}})
		if b.Status == "ok" {
			t.Fatalf("%s accepted the foreign id %q and answered ok: %+v", c.method, c.id, b)
		}
		if b.Error == nil || b.Error.Code != errParameter {
			t.Fatalf("%s with %q: error %+v; want code %d", c.method, c.id, b.Error, errParameter)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Browse shape
// ---------------------------------------------------------------------------

func TestBrowseShapeOverFixtureLibrary(t *testing.T) {
	h := newHarness(t, testPass)

	arts := h.ok(t, "getArtists", nil)
	got := map[string][]string{}
	for _, idx := range arts.Artists.Index {
		for _, a := range idx.Artist {
			got[idx.Name] = append(got[idx.Name], a.Name)
		}
	}
	if len(got["A"]) != 1 || got["A"][0] != "Alpha Band" {
		t.Fatalf("index A = %v, want [Alpha Band]", got["A"])
	}
	if len(got["B"]) != 1 || got["B"][0] != "Beta Choir" {
		t.Fatalf("index B = %v, want [Beta Choir]", got["B"])
	}

	var alphaID string
	for _, a := range arts.Artists.Index[0].Artist {
		if a.Name == "Alpha Band" {
			alphaID = a.ID
			if a.AlbumCount != 1 {
				t.Fatalf("Alpha Band albumCount = %d, want 1", a.AlbumCount)
			}
		}
	}

	ar := h.ok(t, "getArtist", url.Values{"id": {alphaID}})
	if len(ar.Artist.Album) != 1 || ar.Artist.Album[0].Name != "First Light" {
		t.Fatalf("getArtist albums = %+v, want only First Light", ar.Artist.Album)
	}
	if ar.Artist.Album[0].SongCount != 2 {
		t.Fatalf("First Light songCount = %d, want 2", ar.Artist.Album[0].SongCount)
	}

	al := h.ok(t, "getAlbum", url.Values{"id": {ar.Artist.Album[0].ID}})
	if len(al.Album.Song) != 2 {
		t.Fatalf("getAlbum returned %d songs, want 2", len(al.Album.Song))
	}
	if al.Album.Song[0].Title != "Aurora Borealis" || al.Album.Song[0].Track != 1 {
		t.Fatalf("first song = %+v, want Aurora Borealis at track 1", al.Album.Song[0])
	}
	if al.Album.Song[1].Track != 2 {
		t.Fatalf("second song track = %d, want 2", al.Album.Song[1].Track)
	}
	if al.Album.Song[0].Duration != 143 || al.Album.Song[1].Duration != 187 {
		t.Fatalf("song durations = %d, %d seconds; want 143, 187",
			al.Album.Song[0].Duration, al.Album.Song[1].Duration)
	}
	if al.Album.Duration != 330 {
		t.Fatalf("album duration = %d seconds, want 330", al.Album.Duration)
	}
	if al.Album.Year != 2001 {
		t.Fatalf("album year = %d, want 2001", al.Album.Year)
	}
	for _, s := range al.Album.Song {
		if s.Suffix != "wav" || s.ContentType != "audio/wav" {
			t.Fatalf("song %q suffix=%q contentType=%q", s.Title, s.Suffix, s.ContentType)
		}
	}

	// size and offset page the album list.
	all := h.ok(t, "getAlbumList2", url.Values{"size": {"10"}})
	if len(all.AlbumList2.Album) != 2 {
		t.Fatalf("getAlbumList2 returned %d albums, want 2", len(all.AlbumList2.Album))
	}
	first := h.ok(t, "getAlbumList2", url.Values{"size": {"1"}})
	if len(first.AlbumList2.Album) != 1 || first.AlbumList2.Album[0].ID != all.AlbumList2.Album[0].ID {
		t.Fatalf("size=1 returned %+v, want the first album only", first.AlbumList2.Album)
	}
	second := h.ok(t, "getAlbumList2", url.Values{"size": {"1"}, "offset": {"1"}})
	if len(second.AlbumList2.Album) != 1 || second.AlbumList2.Album[0].ID != all.AlbumList2.Album[1].ID {
		t.Fatalf("offset=1 returned %+v, want the second album", second.AlbumList2.Album)
	}
	past := h.ok(t, "getAlbumList2", url.Values{"size": {"10"}, "offset": {"99"}})
	if past.AlbumList2 != nil && len(past.AlbumList2.Album) != 0 {
		t.Fatalf("offset past the end returned %+v", past.AlbumList2.Album)
	}

	// search3 matches a prefix of a title.
	sr := h.ok(t, "search3", url.Values{"query": {"Col"}})
	if len(sr.SearchResult3.Song) != 1 || sr.SearchResult3.Song[0].Title != "Cold Evening" {
		t.Fatalf("search3 Col = %+v, want Cold Evening only", sr.SearchResult3.Song)
	}
}

// ---------------------------------------------------------------------------
// 5. Stars are likes
// ---------------------------------------------------------------------------

func TestStarMapsOntoLikes(t *testing.T) {
	h := newHarness(t, testPass)
	id := songID(h.tracks[1].ID)

	if b := h.ok(t, "getStarred2", nil); b.Starred2 != nil && len(b.Starred2.Song) != 0 {
		t.Fatalf("nothing starred yet, but getStarred2 returned %+v", b.Starred2.Song)
	}

	h.ok(t, "star", url.Values{"id": {id}})
	starred := h.ok(t, "getStarred2", nil)
	if starred.Starred2 == nil || len(starred.Starred2.Song) != 1 || starred.Starred2.Song[0].ID != id {
		t.Fatalf("after star, getStarred2 = %+v, want just %s", starred.Starred2, id)
	}

	h.ok(t, "unstar", url.Values{"id": {id}})
	after := h.ok(t, "getStarred2", nil)
	if after.Starred2 != nil && len(after.Starred2.Song) != 0 {
		t.Fatalf("after unstar, getStarred2 = %+v, want empty", after.Starred2.Song)
	}

	// An album id is not a song id: refuse it rather than starring track 1.
	b := h.json2(t, "star", url.Values{"id": {albumID(1)}})
	if b.Status != "failed" || b.Error == nil || b.Error.Code != errParameter {
		t.Fatalf("star al-1: status %q error %+v; want failed with code %d", b.Status, b.Error, errParameter)
	}
	if again := h.ok(t, "getStarred2", nil); again.Starred2 != nil && len(again.Starred2.Song) != 0 {
		t.Fatalf("star al-1 starred something anyway: %+v", again.Starred2.Song)
	}
}

// ---------------------------------------------------------------------------
// 6. Jukebox and stream are different paths
// ---------------------------------------------------------------------------

func TestJukeboxDrivesTheLocalPlayerAndStreamSendsTheFile(t *testing.T) {
	h := newHarness(t, testPass)
	id := songID(h.tracks[2].ID)

	h.ok(t, "jukeboxControl", url.Values{"action": {"set"}, "id": {id}})

	// The player owns its state in its own goroutine; poll the API for the queue
	// rather than assuming the command has been applied by the time we ask.
	var queue []jSong
	waitFor(t, "the jukebox queue to hold "+id, func() bool {
		b := h.ok(t, "jukeboxControl", url.Values{"action": {"get"}})
		if b.JukeboxPlaylist == nil {
			return false
		}
		queue = b.JukeboxPlaylist.Entry
		return len(queue) == 1 && queue[0].ID == id
	})
	if queue[0].Title != h.tracks[2].Title {
		t.Fatalf("jukebox queue entry = %+v, want %q", queue[0], h.tracks[2].Title)
	}

	h.ok(t, "jukeboxControl", url.Values{"action": {"setGain"}, "gain": {"0.5"}})
	waitFor(t, "gain to read back as 0.5", func() bool {
		b := h.ok(t, "jukeboxControl", url.Values{"action": {"status"}})
		return b.JukeboxStatus != nil && b.JukeboxStatus.Gain == 0.5
	})

	if b := h.json2(t, "jukeboxControl", url.Values{"action": {"levitate"}}); b.Status != "failed" ||
		b.Error == nil || b.Error.Code != errParameter {
		t.Fatalf("unknown jukebox action: status %q error %+v", b.Status, b.Error)
	}

	// stream is the other path: the file's own bytes, to the client's device.
	want, err := os.ReadFile(h.tracks[2].Path)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, "stream", url.Values{"id": {id}})
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("stream Content-Type = %q, want audio/wav", ct)
	}
	if got := rec.Body.Bytes(); len(got) != len(want) || string(got[:44]) != string(want[:44]) {
		t.Fatalf("stream sent %d bytes, want the %d-byte file", len(got), len(want))
	}

	// stream refuses an id that is not a song id.
	var env jEnvelope
	bad := h.do(t, "stream", url.Values{"id": {albumID(1)}, "f": {"json"}})
	if err := json.Unmarshal(bad.Body.Bytes(), &env); err != nil {
		t.Fatalf("stream with an album id did not answer an envelope: %s", bad.Body.String())
	}
	if env.Resp.Status != "failed" || env.Resp.Error == nil || env.Resp.Error.Code != errParameter {
		t.Fatalf("stream al-1: %+v", env.Resp)
	}
}
