package history

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plectra/plectra/internal/match"
	"github.com/plectra/plectra/internal/store"
)

// ImportResult reports what an import did. Skipped rows are duplicates from an
// earlier run; unmatched rows were kept with their raw metadata.
type ImportResult struct {
	Files     int `json:"files"`
	Rows      int `json:"rows"`
	Imported  int `json:"imported"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
}

// gdprEntry covers both shapes Spotify has shipped: the current export uses
// ts/ms_played/master_metadata_*, older ones endTime/msPlayed/artistName.
type gdprEntry struct {
	TS        string `json:"ts"`
	EndTime   string `json:"endTime"`
	MSPlayed  int64  `json:"ms_played"`
	MsPlayed2 int64  `json:"msPlayed"`
	Track     string `json:"master_metadata_track_name"`
	Artist    string `json:"master_metadata_album_artist_name"`
	Album     string `json:"master_metadata_album_album_name"`
	TrackName string `json:"trackName"`
	ArtistOld string `json:"artistName"`
	URI       string `json:"spotify_track_uri"`
}

func (e gdprEntry) played() (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04"} {
		for _, v := range []string{e.TS, e.EndTime} {
			if v == "" {
				continue
			}
			if t, err := time.Parse(layout, v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func (e gdprEntry) fields() (artist, title string, ms int64) {
	artist, title, ms = e.Artist, e.Track, e.MSPlayed
	if artist == "" {
		artist = e.ArtistOld
	}
	if title == "" {
		title = e.TrackName
	}
	if ms == 0 {
		ms = e.MsPlayed2
	}
	return
}

// ImportGDPR reads a Spotify privacy export — a file or a directory of them —
// and records every listen. This is the real historical data: the API only ever
// returns the last fifty plays.
func (r *Recorder) ImportGDPR(ctx context.Context, path string) (ImportResult, error) {
	var res ImportResult
	files, err := historyFiles(path)
	if err != nil {
		return res, err
	}
	if len(files) == 0 {
		return res, fmt.Errorf("no streaming history json found in %s", path)
	}

	index, err := r.index(ctx)
	if err != nil {
		return res, err
	}

	for _, f := range files {
		entries, err := readEntries(f)
		if err != nil {
			// One unreadable file does not cancel the rest of the import.
			continue
		}
		res.Files++

		plays := make([]store.Play, 0, len(entries))
		for _, e := range entries {
			at, ok := e.played()
			if !ok {
				continue
			}
			artist, title, ms := e.fields()
			if title == "" {
				continue // podcast episodes and empty rows carry no track
			}
			res.Rows++

			p := store.Play{
				PlayedAt:       at.Unix(),
				MSPlayed:       ms,
				Source:         store.SourceSpotifyExport,
				RawArtist:      artist,
				RawAlbum:       e.Album,
				RawTitle:       title,
				SpotifyTrackID: strings.TrimPrefix(e.URI, "spotify:track:"),
			}
			if id, ok := index[match.Key(artist, title)]; ok {
				p.TrackID = id
				res.Matched++
			} else {
				res.Unmatched++
			}
			// The export carries no track duration, so completion is judged on
			// play time alone: 100 seconds, about half a typical song.
			p.Completed = ms >= 100*1000
			plays = append(plays, p)
		}

		n, err := r.st.ImportPlays(ctx, plays)
		if err != nil {
			return res, err
		}
		res.Imported += n
	}
	return res, nil
}

func historyFiles(path string) ([]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{path}, nil
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if strings.HasSuffix(name, ".json") && strings.Contains(name, "history") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func readEntries(path string) ([]gdprEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []gdprEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// ExternalPlay is one listen observed somewhere else — today, Spotify's
// recently-played endpoint.
type ExternalPlay struct {
	PlayedAt  time.Time
	MSPlayed  int64
	Artist    string
	Album     string
	Title     string
	SpotifyID string
}

// ImportExternal records listens from a polling bridge. Re-importing overlapping
// windows is safe: duplicates are dropped by the database.
func (r *Recorder) ImportExternal(ctx context.Context, source string, plays []ExternalPlay) (ImportResult, error) {
	var res ImportResult
	index, err := r.index(ctx)
	if err != nil {
		return res, err
	}
	rows := make([]store.Play, 0, len(plays))
	for _, p := range plays {
		res.Rows++
		row := store.Play{
			PlayedAt: p.PlayedAt.Unix(), MSPlayed: p.MSPlayed, Completed: true,
			Source: source, RawArtist: p.Artist, RawAlbum: p.Album, RawTitle: p.Title,
			SpotifyTrackID: p.SpotifyID,
		}
		if id, ok := index[match.Key(p.Artist, p.Title)]; ok {
			row.TrackID = id
			res.Matched++
		} else {
			res.Unmatched++
		}
		rows = append(rows, row)
	}
	n, err := r.st.ImportPlays(ctx, rows)
	res.Imported = n
	return res, err
}

// Rematch claims imported rows whose file has since entered the library, and
// reports how many rows found their track.
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
