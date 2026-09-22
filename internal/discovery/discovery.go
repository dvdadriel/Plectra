// Package discovery suggests what to play next. Every suggestion is a track
// already in the local library — Plectra can only recommend what it can play.
//
// Signals come from listening recorded here, not from an external profile:
// which artists actually get played, what has not been heard in months, what
// was never played at all. External providers only answer "who sounds like
// this", and that answer is immediately resolved back to local tracks.
package discovery

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// Section is one band of suggestions with the reason it exists. A recommender
// that cannot say why it chose something is a slot machine.
type Section struct {
	Title  string        `json:"title"`
	Reason string        `json:"reason"`
	Tracks []store.Track `json:"tracks"`
}

// SimilarProvider answers "who sounds like this artist". None ship with Plectra
// today; discovery runs on local listening alone. The seam stays because the
// section it feeds is the one worth having when a provider does exist.
type SimilarProvider interface {
	Name() string
	// SimilarArtists returns artist names, most similar first. mbid may be
	// empty when the library has not been enriched.
	SimilarArtists(ctx context.Context, name, mbid string, limit int) ([]string, error)
}

type Service struct {
	st        *store.Store
	providers []SimilarProvider
	// Now is injectable so "not heard in a while" is testable without waiting.
	Now func() time.Time
}

func New(st *store.Store, providers ...SimilarProvider) *Service {
	live := providers[:0]
	for _, p := range providers {
		if p != nil {
			live = append(live, p)
		}
	}
	return &Service{st: st, providers: live, Now: time.Now}
}

const (
	seedArtists = 5
	perSection  = 12
	staleMonths = 3
)

// Sections builds the discovery view. Sections with nothing in them are left
// out rather than shown empty.
func (s *Service) Sections(ctx context.Context) ([]Section, error) {
	var out []Section

	top, err := s.st.TopArtists(ctx, seedArtists)
	if err != nil {
		return nil, err
	}

	if sec, err := s.similarToTaste(ctx, top); err != nil {
		log.Printf("discovery: similar artists: %v", err) // the network is optional
	} else if sec != nil {
		out = append(out, *sec)
	}

	stale := s.Now().AddDate(0, -staleMonths, 0).Unix()
	if tracks, err := s.st.TracksNotHeardSince(ctx, stale, perSection); err == nil && len(tracks) > 0 {
		out = append(out, Section{
			Title:  "Not heard in a while",
			Reason: "You played these before, but not in the last three months.",
			Tracks: tracks,
		})
	}

	if tracks, err := s.st.TracksNeverPlayed(ctx, perSection); err == nil && len(tracks) > 0 {
		out = append(out, Section{
			Title:  "Never played",
			Reason: "In your library, never listened to all the way through.",
			Tracks: tracks,
		})
	}

	return out, nil
}

// similarToTaste asks the providers who resembles the artists actually being
// played, then keeps only the ones whose music is already on disk.
func (s *Service) similarToTaste(ctx context.Context, top []store.ArtistAffinity) (*Section, error) {
	if len(s.providers) == 0 || len(top) == 0 {
		return nil, nil
	}

	// Rank candidate artists by how often they come back across seeds: an
	// artist similar to three of your favourites beats one similar to a single
	// play.
	score := map[string]int{}
	seeds := map[string]bool{}
	for _, a := range top {
		seeds[a.Name] = true
	}
	for _, seed := range top {
		for _, p := range s.providers {
			names, err := p.SimilarArtists(ctx, seed.Name, seed.MBID, 25)
			if err != nil {
				log.Printf("discovery: %s: %v", p.Name(), err)
				continue
			}
			for i, name := range names {
				if seeds[name] {
					continue // already a favourite; not a discovery
				}
				score[name] += len(names) - i
			}
		}
	}
	if len(score) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(score))
	for n := range score {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if score[names[i]] != score[names[j]] {
			return score[names[i]] > score[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > 40 {
		names = names[:40]
	}

	tracks, err := s.st.TracksByArtistNames(ctx, names, 2)
	if err != nil {
		return nil, err
	}
	if len(tracks) == 0 {
		return nil, nil // nothing similar is actually in the library
	}
	if len(tracks) > perSection {
		tracks = tracks[:perSection]
	}
	return &Section{
		Title:  "Because you play " + top[0].Name,
		Reason: "Artists that listeners of your most played artists also listen to, filtered to what you own.",
		Tracks: tracks,
	}, nil
}
