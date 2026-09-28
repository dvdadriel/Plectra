package subsonic

import (
	"context"
	"math/rand"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/plectra/plectra/internal/store"
)

func (a *API) toChild(t store.Track, starred map[int64]bool) child {
	c := child{
		ID: songID(t.ID), Parent: albumID(t.AlbumID), IsDir: false,
		Title: t.Title, Album: t.Album, Artist: t.Artist,
		Track: t.TrackNo, DiscNumber: t.DiscNo, Duration: int(t.DurationMS / 1000),
		CoverArt: albumID(t.AlbumID), AlbumID: albumID(t.AlbumID), ArtistID: artistID(t.ArtistID),
		Suffix: strings.TrimPrefix(strings.ToLower(filepath.Ext(t.Path)), "."),
		Type:   "music",
	}
	c.ContentType = contentType(c.Suffix)
	if starred[t.ID] {
		// Subsonic wants a timestamp here; the exact instant is not recorded per
		// star, so the field simply marks "yes".
		c.Starred = time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	return c
}

func contentType(suffix string) string {
	switch suffix {
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	}
	return "application/octet-stream"
}

func (a *API) starredSet(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	ids, err := a.lists.LikedIDs(ctx)
	if err != nil {
		return out // stars are decoration; failing to read them must not fail a browse
	}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// getArtists answers both getArtists and getIndexes: the same alphabetical
// index, under the tag each method expects.
func (a *API) getArtists(ctx context.Context, method string) (response, error) {
	artists, err := a.cat.Artists(ctx)
	if err != nil {
		return response{}, err
	}
	counts, err := a.cat.ArtistAlbumCounts(ctx)
	if err != nil {
		return response{}, err
	}

	groups := map[string][]artist{}
	for _, ar := range artists {
		groups[initial(ar.Name)] = append(groups[initial(ar.Name)], artist{
			ID: artistID(ar.ID), Name: ar.Name, AlbumCount: counts[ar.ID],
		})
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	list := &indexList{LastModified: time.Now().UnixMilli(), IgnoredArticles: "The El La Los Las Le Les"}
	for _, k := range keys {
		list.Index = append(list.Index, index{Name: k, Artist: groups[k]})
	}

	res := okResponse()
	if method == "getIndexes" {
		res.Indexes = list
	} else {
		res.Artists = list
	}
	return res, nil
}

// initial is the index letter an artist is filed under; anything that is not a
// letter lands in "#", which is what clients expect.
func initial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) {
			return strings.ToUpper(string(r))
		}
		if unicode.IsDigit(r) {
			return "#"
		}
	}
	return "#"
}

func (a *API) getArtist(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "ar-")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	artists, err := a.cat.Artists(ctx)
	if err != nil {
		return response{}, err
	}
	var name string
	for _, ar := range artists {
		if ar.ID == id {
			name = ar.Name
		}
	}
	if name == "" {
		return response{}, store.ErrNotFound
	}

	albums, err := a.cat.AlbumsByArtist(ctx, id)
	if err != nil {
		return response{}, err
	}
	out := &artistWithAlbums{artist: artist{ID: artistID(id), Name: name, AlbumCount: len(albums)}}
	for _, al := range albums {
		songs, err := a.cat.AlbumTracks(ctx, al.ID)
		if err != nil {
			return response{}, err
		}
		out.Album = append(out.Album, toAlbumID3(al, songs))
	}

	res := okResponse()
	res.Artist = out
	return res, nil
}

func toAlbumID3(al store.Album, songs []store.Track) albumID3 {
	var duration int64
	var artistRef int64
	for _, s := range songs {
		duration += s.DurationMS
		artistRef = s.ArtistID
	}
	out := albumID3{
		ID: albumID(al.ID), Name: al.Title, Artist: al.Artist,
		CoverArt: albumID(al.ID), SongCount: len(songs),
		Duration: int(duration / 1000), Year: al.Year,
	}
	if artistRef != 0 {
		out.ArtistID = artistID(artistRef)
	}
	return out
}

// getRandomSongs is the shuffle button most clients open with. Tracks with no
// file are left out: the client fetches them over /rest/stream, and a row with
// nothing on disk would be handed to it only to fail.
//
// genre, fromYear, toYear and musicFolderId are accepted and ignored rather
// than answered wrongly; Plectra has one folder and stores no genre.
func (a *API) getRandomSongs(ctx context.Context, r *http.Request) (response, error) {
	tracks, err := a.cat.AllTracks(ctx)
	if err != nil {
		return response{}, err
	}
	playable := tracks[:0]
	for _, t := range tracks {
		if t.Path != "" {
			playable = append(playable, t)
		}
	}
	rand.Shuffle(len(playable), func(i, j int) { playable[i], playable[j] = playable[j], playable[i] })

	size := intParam(r, "size", 10)
	if size > len(playable) {
		size = len(playable)
	}
	starred := a.starredSet(ctx)
	out := &songList{Song: []child{}}
	for _, t := range playable[:size] {
		out.Song = append(out.Song, a.toChild(t, starred))
	}
	res := okResponse()
	res.RandomSongs = out
	return res, nil
}

func (a *API) getAlbum(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "al-")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	al, err := a.cat.Album(ctx, id)
	if err != nil {
		return response{}, err
	}
	songs, err := a.cat.AlbumTracks(ctx, id)
	if err != nil {
		return response{}, err
	}
	starred := a.starredSet(ctx)
	out := &albumWithSongs{albumID3: toAlbumID3(al, songs)}
	for _, s := range songs {
		out.Song = append(out.Song, a.toChild(s, starred))
	}
	res := okResponse()
	res.Album = out
	return res, nil
}

// getAlbumList serves the browse screens. Only the orderings that can be
// answered from local data are supported; the rest fall back to alphabetical
// rather than inventing a ranking.
func (a *API) getAlbumList(ctx context.Context, r *http.Request, method string) (response, error) {
	albums, err := a.cat.Albums(ctx)
	if err != nil {
		return response{}, err
	}
	switch r.Form.Get("type") {
	case "newest":
		sort.SliceStable(albums, func(i, j int) bool { return albums[i].Year > albums[j].Year })
	case "random":
		rand.Shuffle(len(albums), func(i, j int) { albums[i], albums[j] = albums[j], albums[i] })
	case "alphabeticalByArtist", "":
		// Already ordered by artist then year by the catalog query.
	default:
		sort.SliceStable(albums, func(i, j int) bool { return albums[i].Title < albums[j].Title })
	}

	size := intParam(r, "size", 10)
	offset := intParam(r, "offset", 0)
	if offset > len(albums) {
		offset = len(albums)
	}
	end := offset + size
	if end > len(albums) {
		end = len(albums)
	}

	list := &albumList{}
	for _, al := range albums[offset:end] {
		songs, err := a.cat.AlbumTracks(ctx, al.ID)
		if err != nil {
			return response{}, err
		}
		list.Album = append(list.Album, toAlbumID3(al, songs))
	}

	res := okResponse()
	if method == "getAlbumList" {
		res.AlbumList = list
	} else {
		res.AlbumList2 = list
	}
	return res, nil
}

func intParam(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.Form.Get(name))
	if err != nil || v < 0 {
		return def
	}
	return v
}

func (a *API) getSong(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "tr-")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	tracks, err := a.cat.Tracks(ctx, []int64{id})
	if err != nil {
		return response{}, err
	}
	if len(tracks) == 0 {
		return response{}, store.ErrNotFound
	}
	c := a.toChild(tracks[0], a.starredSet(ctx))
	res := okResponse()
	res.Song = &c
	return res, nil
}

func (a *API) search(ctx context.Context, r *http.Request, method string) (response, error) {
	q := r.Form.Get("query")
	found, err := a.cat.Search(ctx, strings.Trim(q, `"`), intParam(r, "songCount", 50))
	if err != nil {
		return response{}, err
	}
	starred := a.starredSet(ctx)

	out := &searchResult{}
	for _, ar := range found.Artists {
		out.Artist = append(out.Artist, artist{ID: artistID(ar.ID), Name: ar.Name})
	}
	for _, al := range found.Albums {
		songs, err := a.cat.AlbumTracks(ctx, al.ID)
		if err != nil {
			return response{}, err
		}
		out.Album = append(out.Album, toAlbumID3(al, songs))
	}
	for _, t := range found.Tracks {
		out.Song = append(out.Song, a.toChild(t, starred))
	}

	res := okResponse()
	if method == "search2" {
		res.SearchResult2 = out
	} else {
		res.SearchResult3 = out
	}
	return res, nil
}
