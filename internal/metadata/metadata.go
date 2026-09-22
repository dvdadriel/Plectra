// Package metadata enriches the library from external providers. Nothing here
// is required for playback: every provider may be absent, slow, or broken.
package metadata

import (
	"context"
	"time"
)

// Kind says what is being looked up.
type Kind string

const (
	KindAlbum  Kind = "album"
	KindArtist Kind = "artist"
	KindTrack  Kind = "track"
)

// Query is what the local library knows about an entity.
type Query struct {
	Kind   Kind
	Artist string
	Album  string
	Title  string
	Year   int
}

// Match is what a provider found. Fields it cannot fill stay empty; the store
// never overwrites a known value with an empty one.
type Match struct {
	ID       string // provider-native id (MBID, Spotify id)
	Artist   string
	Album    string
	Title    string
	Year     int
	CoverURL string
	Score    int // 0-100, provider's own confidence
}

// Provider is where metadata comes from: MusicBrainz, Spotify, local tags.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, q Query) ([]Match, error)
}

// Clock exists so rate limits and backoff can be tested without waiting.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SystemClock is the clock used outside tests.
var SystemClock Clock = realClock{}

// limiter spaces requests at least interval apart. MusicBrainz requires one
// request per second and enforces it; exceeding it gets the client blocked.
type limiter struct {
	clock    Clock
	interval time.Duration
	last     time.Time
}

func newLimiter(clock Clock, interval time.Duration) *limiter {
	return &limiter{clock: clock, interval: interval}
}

func (l *limiter) wait(ctx context.Context) error {
	if !l.last.IsZero() {
		if d := l.interval - l.clock.Now().Sub(l.last); d > 0 {
			if err := l.clock.Sleep(ctx, d); err != nil {
				return err
			}
		}
	}
	l.last = l.clock.Now()
	return nil
}
