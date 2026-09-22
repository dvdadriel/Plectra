// Package source finds playable audio for a track that is not in the library.
//
// The shape is borrowed from nuclear: a provider answers with *candidates*
// (cheap to find, safe to cache), and a candidate is resolved to a stream URL
// only when it is about to play (expensive, and the URL expires). Separating
// the two is what makes a source that can fail survivable.
package source

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/plectra/plectra/internal/match"
)

// Query describes the track being looked for.
type Query struct {
	Artist     string
	Title      string
	DurationMS int64
}

func (q Query) String() string {
	return strings.TrimSpace(q.Artist + " " + q.Title)
}

// Candidate is one possible answer. It is not playable until resolved.
type Candidate struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMS int64  `json:"durationMs"`
	Score      int    `json:"score"`
}

// Provider is an external place audio can come from.
type Provider interface {
	Name() string
	// Available reports whether the provider can be used right now — a missing
	// external tool, for instance. An unavailable provider is skipped silently.
	Available() bool
	Find(ctx context.Context, q Query) ([]Candidate, error)
	// Resolve returns a location the audio package can open. The result is
	// short-lived by nature: resolve immediately before playing, never store it.
	Resolve(ctx context.Context, c Candidate) (string, error)
}

// Registry holds the providers that are usable in this installation.
type Registry struct {
	providers []Provider
}

func NewRegistry(providers ...Provider) *Registry {
	live := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p != nil && p.Available() {
			live = append(live, p)
		}
	}
	return &Registry{providers: live}
}

// Names lists what is actually usable, for the UI to be honest about.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Name())
	}
	return out
}

// Find asks every provider and returns the candidates ranked by how well they
// match what was asked for.
func (r *Registry) Find(ctx context.Context, q Query) ([]Candidate, error) {
	var all []Candidate
	var firstErr error
	for _, p := range r.providers {
		found, err := p.Find(ctx, q)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p.Name(), err)
			}
			continue
		}
		all = append(all, found...)
	}
	if len(all) == 0 {
		return nil, firstErr
	}
	for i := range all {
		all[i].Score = score(q, all[i])
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	return all, nil
}

// Resolve turns a candidate into something audio.Open understands.
func (r *Registry) Resolve(ctx context.Context, c Candidate) (string, error) {
	for _, p := range r.providers {
		if p.Name() == c.Provider {
			return p.Resolve(ctx, c)
		}
	}
	return "", fmt.Errorf("no provider named %q is available", c.Provider)
}

// score rewards a candidate whose title and artist match, and whose length is
// close to the track being replaced. Length is the strongest signal against
// live versions, covers and hour-long compilations.
func score(q Query, c Candidate) int {
	s := 0
	if match.Normalize(match.StripSuffix(q.Title)) == match.Normalize(match.StripSuffix(c.Title)) {
		s += 50
	} else if strings.Contains(match.Normalize(c.Title), match.Normalize(match.StripSuffix(q.Title))) {
		s += 30
	}
	if q.Artist != "" && strings.Contains(match.Normalize(c.Artist+c.Title), match.Normalize(q.Artist)) {
		s += 25
	}
	if q.DurationMS > 0 && c.DurationMS > 0 {
		diff := q.DurationMS - c.DurationMS
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff <= 3000:
			s += 25
		case diff <= 10000:
			s += 10
		case diff > 120000:
			s -= 30 // an hour-long mix is not the song
		}
	}
	return s
}
