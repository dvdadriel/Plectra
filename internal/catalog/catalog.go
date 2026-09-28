// Package catalog answers read queries about the library. It is the only thing
// the API layer is allowed to know about library data.
package catalog

import (
	"context"
	"errors"
	"strings"

	"github.com/plectra/plectra/internal/match"
	"github.com/plectra/plectra/internal/store"
)

type Catalog struct{ st *store.Store }

func New(st *store.Store) *Catalog { return &Catalog{st: st} }

func (c *Catalog) Albums(ctx context.Context) ([]store.Album, error)   { return c.st.Albums(ctx) }
func (c *Catalog) Artists(ctx context.Context) ([]store.Artist, error) { return c.st.Artists(ctx) }

func (c *Catalog) AlbumTracks(ctx context.Context, id int64) ([]store.Track, error) {
	return c.st.TracksByAlbum(ctx, id)
}

func (c *Catalog) ArtistTracks(ctx context.Context, id int64) ([]store.Track, error) {
	return c.st.TracksByArtist(ctx, id)
}

func (c *Catalog) Tracks(ctx context.Context, ids []int64) ([]store.Track, error) {
	return c.st.TracksByIDs(ctx, ids)
}

// AllTracks is the whole library in one slice. Only callers that genuinely
// need every row use it; a personal library fits in memory and the scanner
// already reads it whole.
func (c *Catalog) AllTracks(ctx context.Context) ([]store.Track, error) {
	return c.st.AllTracks(ctx)
}

// SetDuration records a length learned after the row was written, which is how
// a track with no file on disk ever gets one.
func (c *Catalog) SetDuration(ctx context.Context, id int64, ms int64) error {
	return c.st.SetTrackDuration(ctx, id, ms)
}

func (c *Catalog) Search(ctx context.Context, q string, limit int) (store.SearchResult, error) {
	return c.st.Search(ctx, q, limit)
}

func (c *Catalog) Album(ctx context.Context, id int64) (store.Album, error) {
	return c.st.Album(ctx, id)
}

func (c *Catalog) AlbumsByArtist(ctx context.Context, artistID int64) ([]store.Album, error) {
	return c.st.AlbumsByArtist(ctx, artistID)
}

func (c *Catalog) ArtistAlbumCounts(ctx context.Context) (map[int64]int, error) {
	return c.st.ArtistAlbumCounts(ctx)
}

// Cover is the on-disk path of an album's cached artwork.
func (c *Catalog) Cover(ctx context.Context, albumID int64) (string, error) {
	return c.st.AlbumCover(ctx, albumID)
}

// Adopt gives a track Plectra does not have on disk a row of its own, so it can
// be liked, put in a playlist and queued like anything else. The path stays
// empty: that is what tells the player to resolve a stream when it lands on it.
// Adopting the same track twice returns the same row.
func (c *Catalog) Adopt(ctx context.Context, t store.Track) (store.Track, error) {
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		return store.Track{}, errors.New("a title is required")
	}
	t.Artist = strings.TrimSpace(t.Artist)
	if t.Artist == "" {
		t.Artist = "Unknown artist"
	}
	t.Album = strings.TrimSpace(t.Album)
	if t.Album == "" {
		t.Album = "Singles"
	}
	t.Path = ""
	t.FileHash = ExternalHash(t.Artist, t.Album, t.Title)

	id, err := c.st.UpsertTrack(ctx, t)
	if err != nil {
		return store.Track{}, err
	}
	ts, err := c.st.TracksByIDs(ctx, []int64{id})
	if err != nil {
		return store.Track{}, err
	}
	if len(ts) == 0 {
		return store.Track{}, store.ErrNotFound
	}
	return ts[0], nil
}

// ExternalHash is the identity of a track that has no file. The "ext:" prefix
// keeps it out of the space of content hashes the scanner writes.
func ExternalHash(artist, album, title string) string {
	return "ext:" + match.Normalize(artist) + "\x1f" +
		match.Normalize(album) + "\x1f" + match.Normalize(title)
}
