package playlist

import (
	"context"

	"github.com/plectra/plectra/internal/match"
)

// index maps every library track to the normalised key it would be found under,
// so a list of loosely-spelled names can be resolved in one pass.
func (s *Service) index(ctx context.Context) (map[string]int64, error) {
	tracks, err := s.st.AllTracks(ctx)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int64, len(tracks))
	for _, t := range tracks {
		idx[match.Key(t.Artist, t.Title)] = t.ID
	}
	return idx, nil
}

// MatchTracks reports, for each artist/title pair, the local track id or zero.
// It is what lets a catalogue album show which of its songs play from the
// library and which have to be resolved from somewhere else.
func (s *Service) MatchTracks(ctx context.Context, pairs [][2]string) ([]int64, error) {
	index, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(pairs))
	for i, p := range pairs {
		out[i] = index[match.Key(p[0], p[1])]
	}
	return out, nil
}
