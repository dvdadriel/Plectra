package player

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/audio"
	"github.com/plectra/plectra/internal/store"
)

// fakeSink consumes PCM at whatever rate the engine offers it, so the audio
// engine can be tested deterministically without a sound card.
type fakeSink struct {
	mu     sync.Mutex
	played int64
	got    int
}

func (s *fakeSink) Write(pcm []float32) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got += len(pcm)
	s.played += int64(len(pcm) / 2)
	return len(pcm), nil
}
func (s *fakeSink) Format() audio.Format { return audio.Format{SampleRate: 48000, Channels: 2} }
func (s *fakeSink) Played() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.played
}
func (s *fakeSink) Close() error { return nil }

// writeWAV writes a 16-bit stereo sine-ish tone of the given duration.
func writeWAV(t *testing.T, path string, ms int) {
	t.Helper()
	const rate, ch = 44100, 2
	frames := rate * ms / 1000
	data := make([]byte, frames*ch*2)
	for i := 0; i < frames*ch; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(i%1000*30)))
	}
	var b []byte
	put32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	put16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	b = append(b, "RIFF"...)
	put32(uint32(36 + len(data)))
	b = append(b, "WAVEfmt "...)
	put32(16)
	put16(1)
	put16(ch)
	put32(rate)
	put32(rate * ch * 2)
	put16(ch * 2)
	put16(16)
	b = append(b, "data"...)
	put32(uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueAdvancesGaplessAndStops(t *testing.T) {
	dir := t.TempDir()
	var q []store.Track
	for i, name := range []string{"a.wav", "b.wav"} {
		p := filepath.Join(dir, name)
		writeWAV(t, p, 200)
		q = append(q, store.Track{ID: int64(i + 1), Path: p, Title: name, DurationMS: 200})
	}

	sink := &fakeSink{}
	p := New(sink)
	p.Play(q, 0)

	waitFor(t, "second track", func() bool { return p.State().Index == 1 })
	waitFor(t, "playback to finish", func() bool { return !p.State().Playing })

	// 400ms of source audio, resampled 44.1k -> 48k, arrives as ~19200 frames/track.
	if got, want := sink.Played(), int64(2*200*48000/1000); got < want*9/10 {
		t.Fatalf("sink got %d frames, want about %d", got, want)
	}
}

func TestPositionComesFromSinkFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.wav")
	writeWAV(t, path, 2000)
	q := []store.Track{{ID: 1, Path: path, DurationMS: 2000}}

	sink := &fakeSink{}
	p := New(sink)
	p.Play(q, 0)
	waitFor(t, "position past 500ms", func() bool { return p.State().PositionMS > 500 })

	p.Pause()
	st := p.State()
	if st.Playing {
		t.Fatal("pause did not stop playback")
	}
	p.SeekMS(1000)
	waitFor(t, "seek to apply", func() bool {
		pos := p.State().PositionMS
		return pos >= 1000 && pos < 1300 // position reports the seek target, not zero
	})
}

func TestBrokenFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(bad, []byte("not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.wav")
	writeWAV(t, good, 100)

	sink := &fakeSink{}
	p := New(sink)
	p.Play([]store.Track{{ID: 1, Path: bad}, {ID: 2, Path: good}}, 0)

	waitFor(t, "player to skip to the good file", func() bool { return p.State().Index == 1 })
}

func TestRingWrapsWithoutLosingSamples(t *testing.T) {
	r := newRing(8)
	in := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	if n := r.write(in); n != 8 {
		t.Fatalf("wrote %d, want 8", n)
	}
	if n := r.write([]float32{9}); n != 0 {
		t.Fatalf("wrote %d into a full ring, want 0", n)
	}
	out := make([]float32, 8)
	r.read(out)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("out[%d] = %v, want %v", i, out[i], in[i])
		}
	}
	// Wrapping: write past the end of the backing array and read it back.
	r.write([]float32{10, 11, 12})
	got := make([]float32, 3)
	r.read(got)
	if got[0] != 10 || got[2] != 12 {
		t.Fatalf("after wrap got %v", got)
	}
}

func TestRemoveAndClearKeepTheQueueHonest(t *testing.T) {
	dir := t.TempDir()
	var q []store.Track
	for i, name := range []string{"a.wav", "b.wav", "c.wav"} {
		p := filepath.Join(dir, name)
		writeWAV(t, p, 3000)
		q = append(q, store.Track{ID: int64(i + 1), Path: p, Title: name, DurationMS: 3000})
	}

	p := New(&fakeSink{})
	p.Play(q, 1) // playing "b"
	waitFor(t, "playback to start", func() bool { return p.State().Playing })

	// Removing an earlier entry shifts the index but keeps the same track playing.
	p.Remove(0)
	waitFor(t, "index to shift", func() bool { return p.State().Index == 0 })
	if got := p.State().Queue[0].Title; got != "b.wav" {
		t.Fatalf("playing %q after removing an earlier track, want b.wav", got)
	}

	// Removing what is playing moves on to the next track.
	p.Remove(0)
	waitFor(t, "next track", func() bool {
		st := p.State()
		return len(st.Queue) == 1 && st.Queue[0].Title == "c.wav"
	})

	p.Clear()
	waitFor(t, "queue to empty", func() bool {
		st := p.State()
		return len(st.Queue) == 0 && !st.Playing && st.Index == -1
	})
}

func TestPrevOnFirstTrackRestartsInsteadOfEmptyingTheQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.wav")
	writeWAV(t, path, 4000)
	q := []store.Track{{ID: 1, Path: path, Title: "a", DurationMS: 4000}}

	p := New(&fakeSink{})
	p.Play(q, 0)
	waitFor(t, "playback", func() bool { return p.State().Playing })

	p.Prev()
	waitFor(t, "prev to settle", func() bool { return p.State().Index == 0 })
	if st := p.State(); len(st.Queue) != 1 || st.Index != 0 {
		t.Fatalf("after prev on the first track: index %d, queue %d — want index 0 with the queue intact",
			st.Index, len(st.Queue))
	}
}

func TestNextPastTheEndKeepsTheQueue(t *testing.T) {
	dir := t.TempDir()
	var q []store.Track
	for i, name := range []string{"a.wav", "b.wav"} {
		path := filepath.Join(dir, name)
		writeWAV(t, path, 4000)
		q = append(q, store.Track{ID: int64(i + 1), Path: path, Title: name, DurationMS: 4000})
	}

	p := New(&fakeSink{})
	p.Play(q, 1) // already on the last track
	waitFor(t, "playback", func() bool { return p.State().Playing })

	p.Next()
	waitFor(t, "next to settle", func() bool { return !p.State().Playing })
	st := p.State()
	if len(st.Queue) != 2 || st.Index != 1 {
		t.Fatalf("after next past the end: index %d, queue %d — want index 1 with the queue intact",
			st.Index, len(st.Queue))
	}

	// And it must be possible to start again from there.
	p.Resume()
	waitFor(t, "resume", func() bool { return p.State().Playing })
}

// A run of unreadable files used to re-enter gapless() with a nil decoder and
// crash the process. Playback must simply stop.
func TestQueueOfBrokenFilesStopsWithoutCrashing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.wav")
	writeWAV(t, good, 150)

	q := []store.Track{{ID: 1, Path: good, Title: "good", DurationMS: 150}}
	for i := 0; i < 3; i++ {
		bad := filepath.Join(dir, fmt.Sprintf("bad%d.wav", i))
		if err := os.WriteFile(bad, []byte("not audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		q = append(q, store.Track{ID: int64(i + 2), Path: bad, Title: "bad"})
	}

	p := New(&fakeSink{})
	p.Play(q, 0)
	waitFor(t, "playback to stop after the broken run", func() bool { return !p.State().Playing })

	// The engine must still answer: a panicked goroutine would hang this call.
	if st := p.State(); len(st.Queue) != 4 {
		t.Fatalf("queue = %d, want 4", len(st.Queue))
	}
}

// makeQueue writes n playable files and returns them as a queue.
func makeQueue(t *testing.T, n int, ms int) []store.Track {
	t.Helper()
	dir := t.TempDir()
	var q []store.Track
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("t%d.wav", i))
		writeWAV(t, p, ms)
		q = append(q, store.Track{ID: int64(i + 1), Path: p, Title: fmt.Sprintf("T%d", i+1), DurationMS: int64(ms)})
	}
	return q
}

// A queue that runs down is topped up from Suggest, so playback continues with
// no browser open — the whole point of doing this in the player rather than in
// the page.
func TestQueueRefillsItselfBeforeItRunsOut(t *testing.T) {
	q := makeQueue(t, 3, 150)
	extra := makeQueue(t, 2, 150)
	for i := range extra {
		extra[i].ID = int64(100 + i)
		extra[i].Title = fmt.Sprintf("X%d", i+1)
	}

	var mu sync.Mutex
	var seeds []string
	calls := 0

	p := New(&fakeSink{})
	p.Suggest = func(_ context.Context, seed store.Track) []store.Track {
		mu.Lock()
		calls++
		seeds = append(seeds, seed.Title)
		first := calls == 1
		mu.Unlock()
		if !first {
			return nil // top up once; this test is not about doing it forever
		}
		return extra
	}
	p.Play(q, 0)

	waitFor(t, "the queue to grow", func() bool { return len(p.State().Queue) > 3 })

	st := p.State()
	// Appended, never inserted: a queue the listener built keeps its order.
	want := []string{"T1", "T2", "T3", "X1", "X2"}
	for i, w := range want {
		if i >= len(st.Queue) || st.Queue[i].Title != w {
			t.Fatalf("queue = %v, want %v — the refill must append", titlesOf(st.Queue), want)
		}
	}

	mu.Lock()
	gotSeed := len(seeds) > 0 && seeds[0] != ""
	mu.Unlock()
	if !gotSeed {
		t.Error("Suggest was called with no seed track")
	}

	waitFor(t, "playback to reach the appended tracks", func() bool { return p.State().Index >= 3 })
	waitFor(t, "playback to finish", func() bool { return !p.State().Playing })
}

func titlesOf(q []store.Track) []string {
	out := make([]string, len(q))
	for i, t := range q {
		out[i] = t.Title
	}
	return out
}

// Without a Suggest the player must behave exactly as it always has: play to
// the end and stop.
func TestQueueStopsCleanlyWithNoSuggest(t *testing.T) {
	p := New(&fakeSink{})
	p.Play(makeQueue(t, 2, 150), 0)

	waitFor(t, "playback to finish", func() bool { return !p.State().Playing })
	if n := len(p.State().Queue); n != 2 {
		t.Errorf("queue grew to %d with no Suggest set", n)
	}
}

// Repeat means the listener asked for this queue again. Topping it up would
// quietly turn a loop into an endless mix.
func TestNoRefillWhenRepeatIsOn(t *testing.T) {
	var calls atomic.Int32
	p := New(&fakeSink{})
	p.Suggest = func(context.Context, store.Track) []store.Track {
		calls.Add(1)
		return makeQueue(t, 1, 100)
	}
	p.SetMode(false, "all")
	p.Play(makeQueue(t, 2, 120), 0)

	waitFor(t, "the queue to wrap", func() bool { return p.State().Index == 0 && calls.Load() == 0 || calls.Load() > 0 })
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("Suggest was called %d times with repeat on", n)
	}
}

// The guard matters: every state change near the end of a queue would otherwise
// start another lookup.
func TestOnlyOneRefillRunsAtATime(t *testing.T) {
	var concurrent, peak atomic.Int32
	p := New(&fakeSink{})
	p.Suggest = func(context.Context, store.Track) []store.Track {
		n := concurrent.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		concurrent.Add(-1)
		return nil
	}
	p.Play(makeQueue(t, 4, 120), 0)

	waitFor(t, "playback to finish", func() bool { return !p.State().Playing })
	if peak.Load() > 1 {
		t.Errorf("%d refills ran at once; the in-flight guard did not hold", peak.Load())
	}
}
