package history

import (
	"context"

	"github.com/plectra/plectra/internal/match"
)

// Rematch claims imported rows whose file has since entered the library, and
// reports how many rows found their track. Nothing imports history today, but
// rows left by an earlier version are still here and still deserve to find
// their tracks.
func (r *Recorder) Rematch(ctx context.Context) (int, error) {
	unmatched, err := r.st.UnmatchedPlays(ctx)
	if err != nil || len(unmatched) == 0 {
		return 0, err
	}
	index, err := r.index(ctx)
	if err != nil {
		return 0, err
	}

	byTrack := map[int64][]int64{}
	for _, u := range unmatched {
		if id, ok := index[match.Key(u.Artist, u.Title)]; ok {
			byTrack[id] = append(byTrack[id], u.ID)
		}
	}
	claimed := 0
	for trackID, playIDs := range byTrack {
		if err := r.st.ClaimPlays(ctx, trackID, playIDs); err != nil {
			return claimed, err
		}
		claimed += len(playIDs)
	}
	return claimed, nil
}

func (r *Recorder) index(ctx context.Context) (map[string]int64, error) {
	tracks, err := r.st.AllTracks(ctx)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int64, len(tracks))
	for _, t := range tracks {
		idx[match.Key(t.Artist, t.Title)] = t.ID
	}
	return idx, nil
}
