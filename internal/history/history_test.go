package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/store"
)

func TestRecordsOnePlayPerTrackWithCompletion(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	var queue []store.Track
	for _, title := range []string{"A", "B"} {
		id, err := st.UpsertTrack(ctx, store.Track{
			Title: title, Artist: "Artist", Album: "Album", DiscNo: 1,
			Path: "/music/" + title + ".flac", FileHash: title, DurationMS: 200000,
		})
		if err != nil {
			t.Fatal(err)
		}
		queue = append(queue, store.Track{ID: id, Title: title, DurationMS: 200000})
	}
	events := make(chan player.Event, 16)
	send := func(index int, pos int64, playing bool) {
		st := player.State{Playing: playing, Index: index, PositionMS: pos, Queue: queue}
		events <- player.Event{Type: "state", State: &st}
	}

	// Track A is listened through; track B is skipped after two seconds.
	send(0, 0, true)
	send(0, 120000, true)
	send(1, 0, true)
	send(1, 2000, true)
	close(events)

	New(st).Watch(ctx, events)

	// A crossed the halfway mark, B did not.
	if n, _ := st.PlayCount(ctx, queue[0].ID); n != 1 {
		t.Fatalf("completed plays for A = %d, want 1", n)
	}
	if n, _ := st.PlayCount(ctx, queue[1].ID); n != 0 {
		t.Fatalf("completed plays for B = %d, want 0", n)
	}
	// Both are still recorded as events: a skip is data, not noise.
	recent, err := st.RecentPlays(ctx, 10)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent = %v, err = %v", recent, err)
	}
}

func TestCompletionThreshold(t *testing.T) {
	cases := []struct {
		msPlayed, duration int64
		want               bool
	}{
		{0, 200000, false},
		{99999, 200000, false},
		{100000, 200000, true}, // exactly half
		{250000, 900000, true}, // over four minutes of a long track
		{5000, 0, false},       // unknown duration never counts as complete
	}
	for _, c := range cases {
		if got := completed(c.msPlayed, c.duration); got != c.want {
			t.Errorf("completed(%d, %d) = %v, want %v", c.msPlayed, c.duration, got, c.want)
		}
	}
}

// fakeScrobbler stands in for ListenBrainz: it records what the recorder handed
// it and can be told to fail. The concrete client is deliberately not used —
// this is about the wiring, not about HTTP.
type fakeScrobbler struct {
	got []store.Play
	err error
}

func (f *fakeScrobbler) Scrobble(_ context.Context, p store.Play) error {
	f.got = append(f.got, p)
	return f.err
}

// play drives one track through the recorder from 0 to pos and returns the
// store it was written to. Watch is called synchronously on a closed channel,
// so there is nothing to wait for.
func playOnce(t *testing.T, sc Scrobbler, pos int64) (*store.Store, int64) {
	t.Helper()
	st := newStore(t)
	id := addTrack(t, st, "Artist", "Track")
	queue := []store.Track{{ID: id, Title: "Track", Artist: "Artist", DurationMS: 200000}}

	events := make(chan player.Event, 4)
	for _, p := range []int64{0, pos} {
		s := player.State{Playing: true, Index: 0, PositionMS: p, Queue: queue}
		events <- player.Event{Type: "state", State: &s}
	}
	close(events)

	New(st).WithScrobbler(sc).Watch(context.Background(), events)
	return st, id
}

func TestScrobblerReceivesTheCompletedPlay(t *testing.T) {
	f := &fakeScrobbler{}
	st, id := playOnce(t, f, 150000) // past half of 200000 ms

	if len(f.got) != 1 {
		t.Fatalf("scrobbler got %d plays, want 1", len(f.got))
	}
	p := f.got[0]
	if !p.Completed {
		t.Fatalf("scrobbled play = %+v, want completed", p)
	}
	if p.TrackID != id || p.RawArtist != "Artist" || p.RawTitle != "Track" || p.MSPlayed != 150000 {
		t.Fatalf("scrobbled play = %+v, want the track that was heard", p)
	}
	if p.Source != SourcePlectra {
		t.Fatalf("scrobbled source = %q, want %q", p.Source, SourcePlectra)
	}
	if n, err := st.PlayCount(context.Background(), id); err != nil || n != 1 {
		t.Fatalf("stored completed plays = %d (err %v), want 1", n, err)
	}
}

// A skip is data. It is still written, and it reaches the scrobbler flagged
// incomplete — deciding not to submit it belongs to the scrobbler, not here.
func TestIncompletePlayIsStoredAndFlaggedIncomplete(t *testing.T) {
	f := &fakeScrobbler{}
	st, id := playOnce(t, f, 3000) // three seconds of a 200 s track
	ctx := context.Background()

	if n, err := st.PlayCount(ctx, id); err != nil || n != 0 {
		t.Fatalf("completed plays = %d (err %v), want 0 for a skip", n, err)
	}
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 || stats.BySource[SourcePlectra] != 1 {
		t.Fatalf("stats = %+v, want the skip stored once under %q", stats, SourcePlectra)
	}
	if len(f.got) != 1 || f.got[0].Completed {
		t.Fatalf("scrobbler got %+v, want one play flagged incomplete", f.got)
	}
}

// A scrobbler that is down must not cost the listener their history.
func TestScrobbleFailureStillWritesThePlay(t *testing.T) {
	f := &fakeScrobbler{err: errors.New("listenbrainz: status 503")}
	st, id := playOnce(t, f, 150000)

	if len(f.got) != 1 {
		t.Fatalf("scrobbler got %d plays, want 1", len(f.got))
	}
	if n, err := st.PlayCount(context.Background(), id); err != nil || n != 1 {
		t.Fatalf("stored completed plays = %d (err %v), want 1 despite the scrobble error", n, err)
	}
}
