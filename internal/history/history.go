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

// completed is the scrobble convention: half the track, or four minutes.
func completed(msPlayed, durationMS int64) bool {
	if msPlayed >= 4*60*1000 {
		return true
	}
	return durationMS > 0 && msPlayed*2 >= durationMS
}

type Recorder struct {
	st *store.Store

	// observed state of the track currently being listened to
	cur    store.Track
	curPos int64
	active bool
}

func New(st *store.Store) *Recorder { return &Recorder{st: st} }

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
}
