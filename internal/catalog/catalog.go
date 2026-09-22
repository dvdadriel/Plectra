// Package catalog answers read queries about the library. It is the only thing
// the API layer is allowed to know about library data.
package catalog

import (
	"context"

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

func (c *Catalog) Search(ctx context.Context, q string, limit int) (store.SearchResult, error) {
	return c.st.Search(ctx, q, limit)
}

// Cover is the on-disk path of an album's cached artwork.
func (c *Catalog) Cover(ctx context.Context, albumID int64) (string, error) {
	return c.st.AlbumCover(ctx, albumID)
}
