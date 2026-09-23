package discovery

import (
	"context"
	"log"

	"github.com/plectra/plectra/internal/store"
)

// NextUp returns tracks to keep playing once a queue runs down, so a listener
// who queued one album is not met with silence.
//
// It suggests from the same ranking the Discover view uses, minus anything the
// queue already holds — repeating a track that is two places ahead would be a
// worse suggestion than any of the alternatives.
//
// Returning nothing is a valid answer: a library with no listening history has
// nothing honest to suggest, and silence beats a random file.
func (s *Service) NextUp(ctx context.Context, exclude []store.Track, limit int) []store.Track {
	secs, err := s.Sections(ctx)
	if err != nil {
		log.Printf("next up: %v", err)
		return nil
	}

	seen := map[int64]bool{}
	for _, t := range exclude {
		seen[t.ID] = true
	}

	var out []store.Track
	for _, sec := range secs {
		for _, t := range sec.Tracks {
			if seen[t.ID] || t.ID == 0 {
				continue
			}
			seen[t.ID] = true
			out = append(out, t)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}
