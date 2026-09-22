// Package playlist owns playlists and likes. The API layer talks to it, never
// to the tables behind it.
package playlist

import (
	"context"

	"github.com/plectra/plectra/internal/store"
)

type Service struct{ st *store.Store }

func New(st *store.Store) *Service { return &Service{st: st} }

func (s *Service) List(ctx context.Context) ([]store.Playlist, error) { return s.st.Playlists(ctx) }

func (s *Service) Create(ctx context.Context, name, desc string) (store.Playlist, error) {
	return s.st.CreatePlaylist(ctx, name, desc)
}

func (s *Service) Update(ctx context.Context, id int64, name, desc string) error {
	return s.st.UpdatePlaylist(ctx, id, name, desc)
}

func (s *Service) Delete(ctx context.Context, id int64) error { return s.st.DeletePlaylist(ctx, id) }

func (s *Service) Tracks(ctx context.Context, id int64) ([]store.Track, error) {
	return s.st.PlaylistTracks(ctx, id)
}

func (s *Service) Add(ctx context.Context, id int64, trackIDs []int64) error {
	return s.st.AddToPlaylist(ctx, id, trackIDs)
}

func (s *Service) Remove(ctx context.Context, id, trackID int64) error {
	return s.st.RemoveFromPlaylist(ctx, id, trackID)
}

func (s *Service) Reorder(ctx context.Context, id int64, trackIDs []int64) error {
	return s.st.ReorderPlaylist(ctx, id, trackIDs)
}

func (s *Service) Like(ctx context.Context, trackID int64) error   { return s.st.Like(ctx, trackID) }
func (s *Service) Unlike(ctx context.Context, trackID int64) error { return s.st.Unlike(ctx, trackID) }

func (s *Service) Liked(ctx context.Context) ([]store.Track, error) { return s.st.LikedTracks(ctx) }
func (s *Service) LikedIDs(ctx context.Context) ([]int64, error)    { return s.st.LikedIDs(ctx) }
