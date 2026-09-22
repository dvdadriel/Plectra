package history

import (
	"context"
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
