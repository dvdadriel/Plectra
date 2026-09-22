package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

// The last track of a queue used to go unrecorded until the process exited,
// which is exactly the listening a recommender needs most.
func TestTrackThatEndsTheQueueIsBankedImmediately(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	id, err := st.UpsertTrack(ctx, store.Track{
		Title: "Only", Artist: "A", Album: "Alb", DiscNo: 1,
		Path: "/m/only.flac", FileHash: "h", DurationMS: 200000,
	})
	if err != nil {
		t.Fatal(err)
	}
	queue := []store.Track{{ID: id, Title: "Only", DurationMS: 200000}}

	events := make(chan player.Event, 8)
	send := func(pos int64, playing bool) {
		s := player.State{Playing: playing, Index: 0, PositionMS: pos, Queue: queue}
		events <- player.Event{Type: "state", State: &s}
	}
	send(0, true)
	send(120000, true)
	send(199000, true)
	send(199500, false) // played to the end, queue over

	done := make(chan struct{})
	rec := New(st)
	go func() { rec.Watch(ctx, events); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := st.PlayCount(ctx, id); n == 1 {
			close(events)
			<-done
			// And it is banked exactly once, not again at shutdown.
			if n, _ := st.PlayCount(ctx, id); n != 1 {
				t.Fatalf("play count = %d after shutdown, want 1", n)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(events)
	<-done
	n, _ := st.PlayCount(ctx, id)
	t.Fatalf("the finished track was not recorded while running (count = %d)", n)
}

// Pausing in the middle is not the end of a listen.
func TestPauseMidTrackDoesNotBankAPlay(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	id, err := st.UpsertTrack(ctx, store.Track{
		Title: "Only", Artist: "A", Album: "Alb", DiscNo: 1,
		Path: "/m/only.flac", FileHash: "h", DurationMS: 200000,
	})
	if err != nil {
		t.Fatal(err)
	}
	queue := []store.Track{{ID: id, Title: "Only", DurationMS: 200000}}

	events := make(chan player.Event, 8)
	for _, ev := range []struct {
		pos     int64
		playing bool
	}{{0, true}, {40000, true}, {40000, false}, {40000, false}} {
		s := player.State{Playing: ev.playing, Index: 0, PositionMS: ev.pos, Queue: queue}
		events <- player.Event{Type: "state", State: &s}
	}
	close(events)
	New(st).Watch(ctx, events)

	// One row from the shutdown flush, and it is not a completed play.
	if n, _ := st.PlayCount(ctx, id); n != 0 {
		t.Fatalf("completed plays = %d, want 0 — a pause is not a finished listen", n)
	}
	// Exactly one row: the partial listen banked at shutdown. Counting rows,
	// not distinct tracks — a pause that banked early would add a second.
	stats, err := st.HistoryStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 {
		t.Fatalf("recorded %d play rows, want the single partial listen", stats.Total)
	}
}
