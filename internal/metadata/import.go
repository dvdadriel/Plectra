package metadata

import (
	"context"
	"log"

	"github.com/plectra/plectra/internal/match"
)

// ImportResult says what an import did. Unmatched entries are reported rather
// than hidden: the user should know what their library is missing.
type ImportResult struct {
	Playlists int      `json:"playlists"`
	Matched   int      `json:"matched"`
	Unmatched []string `json:"unmatched"`
	// Skipped names playlists Spotify would not hand over. Since 27 November
	// 2024 its algorithmic and editorial playlists — Discover Weekly, Release
	// Radar, and everything Spotify made itself — are closed to applications
	// registered after that date, even though they still appear in the listing.
	Skipped []string `json:"skipped,omitempty"`
}

// ImportPlaylists copies the user's Spotify playlists into local ones, keeping
// only the tracks that exist in the local library — a playlist entry that points
// at nothing playable would be a lie.
func (s *Spotify) ImportPlaylists(ctx context.Context) (ImportResult, error) {
	var res ImportResult
	index, err := s.buildIndex(ctx)
	if err != nil {
		return res, err
	}

	next := "/me/playlists?limit=50"
	for next != "" {
		var page struct {
			Items []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := s.get(ctx, next, &page); err != nil {
			return res, err
		}
		for _, pl := range page.Items {
			ids, missing, err := s.playlistTrackIDs(ctx, pl.ID, index)
			if err != nil {
				// One playlist Spotify refuses must not abort the rest of the
				// import: a single Discover Weekly in the listing would
				// otherwise cost the user every playlist they do own.
				log.Printf("spotify: playlist %q: %v", pl.Name, err)
				res.Skipped = append(res.Skipped, pl.Name)
				continue
			}
			res.Unmatched = append(res.Unmatched, missing...)
			if len(ids) == 0 {
				continue // nothing of this playlist exists locally
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
		next = page.Next
	}
	return res, nil
}

// ImportLiked turns Spotify's liked songs into local likes.
func (s *Spotify) ImportLiked(ctx context.Context) (ImportResult, error) {
	var res ImportResult
	index, err := s.buildIndex(ctx)
	if err != nil {
		return res, err
	}

	next := "/me/tracks?limit=50"
	for next != "" {
		var page struct {
			Items []struct {
				Track spotifyTrack `json:"track"`
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := s.get(ctx, next, &page); err != nil {
			return res, err
		}
		for _, item := range page.Items {
			id, ok := index.find(item.Track)
			if !ok {
				res.Unmatched = append(res.Unmatched, describe(item.Track))
				continue
			}
			if err := s.st.Like(ctx, id); err != nil {
				return res, err
			}
			res.Matched++
		}
		next = page.Next
	}
	return res, nil
}

func (s *Spotify) playlistTrackIDs(ctx context.Context, playlistID string, index *trackIndex) ([]int64, []string, error) {
	var ids []int64
	var missing []string
	next := "/playlists/" + playlistID + "/tracks?limit=100"
	for next != "" {
		var page struct {
			Items []struct {
				Track spotifyTrack `json:"track"`
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := s.get(ctx, next, &page); err != nil {
			return nil, nil, err
		}
		for _, item := range page.Items {
			if item.Track.ID == "" {
				continue // removed or local-only entry
			}
			if id, ok := index.find(item.Track); ok {
				ids = append(ids, id)
			} else {
				missing = append(missing, describe(item.Track))
			}
		}
		next = page.Next
	}
	return ids, missing, nil
}

func describe(t spotifyTrack) string {
	artist := ""
	if len(t.Artists) > 0 {
		artist = t.Artists[0].Name
	}
	return artist + " — " + t.Name
}

// trackIndex matches Spotify tracks against the local library by normalised
// artist and title. Matching on exact strings fails on punctuation and case,
// which is most of real-world tag data.
type trackIndex struct{ byKey map[string]int64 }

func (s *Spotify) buildIndex(ctx context.Context) (*trackIndex, error) {
	tracks, err := s.st.AllTracks(ctx)
	if err != nil {
		return nil, err
	}
	idx := &trackIndex{byKey: make(map[string]int64, len(tracks))}
	for _, t := range tracks {
		idx.byKey[match.Key(t.Artist, t.Title)] = t.ID
	}
	return idx, nil
}

func (i *trackIndex) find(t spotifyTrack) (int64, bool) {
	for _, a := range t.Artists {
		if id, ok := i.byKey[match.Key(a.Name, t.Name)]; ok {
			return id, true
		}
	}
	return 0, false
}
