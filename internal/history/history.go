// Package history records what was actually listened to. It observes the player
// through its event stream — the player knows nothing about it.
package history

import (
	"context"
	"log"
	"time"

	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/store"
)

// SourcePlectra marks plays produced by this player, as opposed to imported ones.
const SourcePlectra = "plectra"

// reachedEnd reports that playback stopped at the end of the track rather than
// somewhere in the middle. Pausing mid-track is not the end of a listen.
// A track with no known duration cannot be judged this way.
func reachedEnd(msPlayed, durationMS int64) bool {
	const slack = 3000 // the ring buffer drains a moment after the last sample
	return durationMS > 0 && msPlayed+slack >= durationMS
}

// completed is the scrobble convention: half the track, or four minutes.
func completed(msPlayed, durationMS int64) bool {
	if msPlayed >= 4*60*1000 {
		return true
	}
	return durationMS > 0 && msPlayed*2 >= durationMS
}

type Recorder struct {
	st        *store.Store
	scrobbler Scrobbler

	// observed state of the track currently being listened to
	cur    store.Track
	curPos int64
	active bool
}

func New(st *store.Store) *Recorder { return &Recorder{st: st} }

// WithScrobbler sends completed plays onward as well as storing them.
func (r *Recorder) WithScrobbler(s Scrobbler) *Recorder {
	r.scrobbler = s
	return r
}

// Stats reports what the history holds, for the UI.
func (r *Recorder) Stats(ctx context.Context) (store.HistoryStats, error) {
	return r.st.HistoryStats(ctx)
}

// Watch consumes player state until ctx is done, writing one plays row per track
// that stops being the current one. A skipped track is recorded too, with
// completed = false: throwing it away would make the statistics lie.
func (r *Recorder) Watch(ctx context.Context, events <-chan player.Event) {
	for {
		select {
		case <-ctx.Done():
			// Shutdown: the incoming context is already cancelled, and the last
			// track still has to be banked, so the write gets a fresh one.
			r.flush(context.WithoutCancel(ctx))
			return
		case ev, ok := <-events:
			if !ok {
				r.flush(ctx)
				return
			}
			if ev.State != nil {
				r.observe(ctx, *ev.State)
			}
		}
	}
}

func (r *Recorder) observe(ctx context.Context, st player.State) {
	var now store.Track
	if st.Index >= 0 && st.Index < len(st.Queue) {
		now = st.Queue[st.Index]
	}

	switch {
	case !r.active && now.ID != 0:
		r.cur, r.curPos, r.active = now, st.PositionMS, true
	case r.active && now.ID != r.cur.ID:
		r.flush(ctx) // the track changed; bank what was heard of the old one
		if now.ID != 0 {
			r.cur, r.curPos, r.active = now, st.PositionMS, true
		}
	case r.active:
		if st.PositionMS > r.curPos {
			r.curPos = st.PositionMS // a seek backwards must not shrink what was heard
		}
		// A track that plays to its end and stops there is finished listening,
		// even though no other track follows. Without this, the last track of a
		// queue is only banked when the process exits.
		if !st.Playing && reachedEnd(r.curPos, r.cur.DurationMS) {
			r.flush(ctx)
		}
	}
}

func (r *Recorder) flush(ctx context.Context) {
	if !r.active {
		return
	}
	p := store.Play{
		TrackID:   r.cur.ID,
		PlayedAt:  time.Now().Unix(),
		MSPlayed:  r.curPos,
		Completed: completed(r.curPos, r.cur.DurationMS),
		Source:    SourcePlectra,
		RawArtist: r.cur.Artist,
		RawAlbum:  r.cur.Album,
		RawTitle:  r.cur.Title,
	}
	r.active = false
	if err := r.st.AddPlay(ctx, p); err != nil {
		log.Printf("history: %v", err) // a failed write never touches playback
	}
	if r.scrobbler != nil {
		if err := r.scrobbler.Scrobble(ctx, p); err != nil {
			log.Printf("scrobble: %v", err) // the listen is stored either way
		}
	}
}

// RecordPlay stores a listen reported by a third-party client. A "now playing"
// notification (submission=false) is not a listen and is deliberately dropped.
func (r *Recorder) RecordPlay(ctx context.Context, trackID int64, submission bool) error {
	if !submission {
		return nil
	}
	tracks, err := r.st.TracksByIDs(ctx, []int64{trackID})
	if err != nil {
		return err
	}
	if len(tracks) == 0 {
		return store.ErrNotFound
	}
	t := tracks[0]
	p := store.Play{
		TrackID: t.ID, PlayedAt: time.Now().Unix(), MSPlayed: t.DurationMS,
		Completed: true, Source: SourcePlectra,
		RawArtist: t.Artist, RawAlbum: t.Album, RawTitle: t.Title,
	}
	if err := r.st.AddPlay(ctx, p); err != nil {
		return err
	}
	if r.scrobbler != nil {
		if err := r.scrobbler.Scrobble(ctx, p); err != nil {
			log.Printf("scrobble: %v", err)
		}
	}
	return nil
}
