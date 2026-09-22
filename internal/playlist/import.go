package playlist

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/plectra/plectra/internal/match"
)

// Export is the file produced by docs/spotify-export.js: playlists read off the
// Spotify web page in the user's own browser. No API, no token — which is why
// the shape is ours and not Spotify's.
type Export struct {
	Source    string           `json:"source"`
	Playlists []ExportPlaylist `json:"playlists"`
	Liked     []ExportTrack    `json:"liked,omitempty"`
}

type ExportPlaylist struct {
	Name      string        `json:"name"`
	SpotifyID string        `json:"spotifyId"`
	Tracks    []ExportTrack `json:"tracks"`
}

type ExportTrack struct {
	Name    string   `json:"name"`
	Artists []string `json:"artists"`
	Album   string   `json:"album"`
}

// ImportResult reports what happened, including what could not be matched.
// Hiding the misses would make the import look better than it was.
type ImportResult struct {
	Playlists int      `json:"playlists"`
	Matched   int      `json:"matched"`
	Liked     int      `json:"liked"`
	Unmatched []string `json:"unmatched"`
	Empty     []string `json:"empty,omitempty"`
}

// ImportFile reads an export and creates local playlists from it, keeping only
// the tracks whose files are in the library — a playlist entry that points at
// nothing playable would be a lie.
func (s *Service) ImportFile(ctx context.Context, path string) (ImportResult, error) {
	var res ImportResult

	b, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	var exp Export
	if err := json.Unmarshal(b, &exp); err != nil {
		return res, fmt.Errorf("%s is not a Plectra export file: %w", path, err)
	}
	if len(exp.Playlists) == 0 && len(exp.Liked) == 0 {
		return res, fmt.Errorf("%s contains no playlists", path)
	}

	index, err := s.index(ctx)
	if err != nil {
		return res, err
	}

	for _, pl := range exp.Playlists {
		var ids []int64
		for _, t := range pl.Tracks {
			if id, ok := lookup(index, t); ok {
				ids = append(ids, id)
			} else {
				res.Unmatched = append(res.Unmatched, describe(t))
			}
		}
		if len(ids) == 0 {
			// Nothing of this playlist exists locally: record the name so the
			// user knows it was seen, but do not create an empty list.
			res.Empty = append(res.Empty, pl.Name)
			continue
		}
		created, err := s.st.CreatePlaylist(ctx, pl.Name, "Imported from Spotify")
		if err != nil {
			return res, err
		}
		if err := s.st.AddToPlaylist(ctx, created.ID, ids); err != nil {
			return res, err
		}
		res.Playlists++
		res.Matched += len(ids)
	}

	for _, t := range exp.Liked {
		id, ok := lookup(index, t)
		if !ok {
			res.Unmatched = append(res.Unmatched, describe(t))
			continue
		}
		if err := s.st.Like(ctx, id); err != nil {
			return res, err
		}
		res.Liked++
	}
	return res, nil
}

// lookup tries every credited artist: a track filed locally under the featured
// artist should still match.
func lookup(index map[string]int64, t ExportTrack) (int64, bool) {
	for _, artist := range t.Artists {
		if id, ok := index[match.Key(artist, t.Name)]; ok {
			return id, true
		}
	}
	return 0, false
}

func describe(t ExportTrack) string {
	return strings.Join(t.Artists, ", ") + " — " + t.Name
}

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
