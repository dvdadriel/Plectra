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

// The fake consumes instantly, so there is never a queued tail to drop.
func (s *fakeSink) Discard() {}

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
	// The guarantee is that the position reports the seek target rather than
	// restarting at zero. An upper bound cannot be asserted here: this sink
	// consumes as fast as it is offered, so the position climbs the instant
	// playback resumes and any window would be a race against the scheduler.
	p.SeekMS(1000)
	waitFor(t, "seek to apply", func() bool { return p.State().PositionMS >= 1000 })
	if got := p.State().PositionMS; got < 1000 {
		t.Errorf("position %dms after seeking to 1000ms", got)
	}
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

// countingSink records how often queued audio was thrown away, which is the
// difference between a skip you hear now and one you hear half a second late.
// It consumes at full speed, so playback advances on its own.
type countingSink struct {
	fakeSink
	discards atomic.Int32
}

func (s *countingSink) Discard() { s.discards.Add(1) }

// frozenSink counts discards but plays nothing, so the index moves only when a
// command moves it. Tests about next and previous need that: with a sink that
// drains instantly, a 300ms track ends before the assertion runs and the queue
// walks itself.
type frozenSink struct {
	stalledSink
	discards atomic.Int32
}

func (s *frozenSink) Discard() { s.discards.Add(1) }

// Skipping must drop what the device still holds. Without it the old track
// keeps playing while the engine has already moved on — which is what made
// next and previous look stuck.
func TestSkipDropsTheBufferedTail(t *testing.T) {
	q := makeQueue(t, 3, 400)
	sink := &frozenSink{}
	p := New(sink)
	p.Play(q, 0)
	waitFor(t, "playback", func() bool { return p.State().Playing })

	before := sink.discards.Load()
	p.Next()
	waitFor(t, "the second track", func() bool { return p.State().Index == 1 })
	if sink.discards.Load() <= before {
		t.Error("Next did not discard the buffered tail")
	}

	before = sink.discards.Load()
	p.Prev()
	waitFor(t, "the first track", func() bool { return p.State().Index == 0 })
	if sink.discards.Load() <= before {
		t.Error("Prev did not discard the buffered tail")
	}
}

// Next must actually reach the following track rather than stalling on the one
// that is playing.
func TestNextAndPrevWalkTheQueue(t *testing.T) {
	p := New(&frozenSink{})
	p.Play(makeQueue(t, 4, 300), 0)
	waitFor(t, "playback", func() bool { return p.State().Playing })

	for want := 1; want <= 3; want++ {
		p.Next()
		waitFor(t, fmt.Sprintf("track %d", want), func() bool { return p.State().Index == want })
	}
	// Previous steps back when it is pressed early in a track.
	for want := 2; want >= 0; want-- {
		p.Prev()
		waitFor(t, fmt.Sprintf("back to track %d", want), func() bool { return p.State().Index == want })
	}
}

// Pause has to be silent at once, and resume has to pick up where the sound
// actually stopped rather than half a second later.
func TestPauseIsImmediateAndResumeDoesNotSkip(t *testing.T) {
	sink := &countingSink{}
	p := New(sink)
	p.Play(makeQueue(t, 1, 3000), 0)
	waitFor(t, "playback", func() bool { return p.State().Playing })
	waitFor(t, "some audio to be consumed", func() bool { return p.State().PositionMS > 0 })

	before := sink.discards.Load()
	p.Pause()
	waitFor(t, "paused", func() bool { return !p.State().Playing })
	if sink.discards.Load() <= before {
		t.Error("pause left the buffered tail playing")
	}
	at := p.State().PositionMS

	p.Resume()
	waitFor(t, "playing again", func() bool { return p.State().Playing })

	// The guarantee is that resume picks the track up where it stopped: it must
	// not restart from the beginning, and it must not skip the half second that
	// pause threw away. An exact millisecond is not assertable here — this sink
	// consumes as fast as it is offered, so the position climbs the moment
	// playback resumes.
	st := p.State()
	if st.Index != 0 {
		t.Errorf("resume moved to track %d; it must stay on the paused one", st.Index)
	}
	if st.PositionMS < at {
		t.Errorf("resumed at %dms, behind the %dms where it paused — the track restarted", st.PositionMS, at)
	}
}

// A read must be answered after the writes already sent, not alongside them.
// While state travelled its own channel the engine's select could serve a
// State() before the Play() that was queued first, so an endpoint answering
// with the player's state described the moment before its own command.
func TestStateReflectsCommandsAlreadySent(t *testing.T) {
	q := makeQueue(t, 3, 200)
	for i := 0; i < 200; i++ {
		// A sink that consumes nothing, so playback cannot advance past the
		// track Play chose: any movement in the index would be the race, not
		// the music.
		p := New(&stalledSink{})
		p.Play(q, 1)
		st := p.State() // no waiting: this must already see the Play
		if st.Index != 1 || len(st.Queue) != 3 {
			t.Fatalf("attempt %d: state is index %d over %d tracks; want index 1 over 3",
				i, st.Index, len(st.Queue))
		}
	}
}

// stalledSink accepts nothing and plays nothing, freezing the engine wherever a
// command left it.
type stalledSink struct{}

func (stalledSink) Write([]float32) (int, error) { return 0, nil }
func (stalledSink) Format() audio.Format         { return audio.Format{SampleRate: 48000, Channels: 2} }
func (stalledSink) Played() int64                { return 0 }
func (stalledSink) Discard()                     {}
func (stalledSink) Close() error                 { return nil }

// A catalogue album is queued with names but no files. Next and previous must
// walk it exactly as they walk a local album — the entry is looked up when it
// comes up, which is why this exists at all.
func TestUnresolvedEntriesAreLookedUpWhenTheyComeUp(t *testing.T) {
	real := makeQueue(t, 3, 400)

	// Two named-only entries around one real file.
	q := []store.Track{
		{ID: 1, Path: real[0].Path, Title: "One", Artist: "A"},
		{Title: "Two", Artist: "A"},   // no file: must be resolved
		{Title: "Three", Artist: "A"}, // no file: must be resolved
	}

	var mu sync.Mutex
	var asked []string
	p := New(&frozenSink{})
	p.Resolve = func(_ context.Context, tr store.Track) (string, error) {
		mu.Lock()
		asked = append(asked, tr.Title)
		mu.Unlock()
		return real[1].Path, nil
	}
	p.Play(q, 0)
	waitFor(t, "the first track", func() bool { return p.State().Index == 0 })

	p.Next()
	waitFor(t, "the looked-up track to play", func() bool {
		st := p.State()
		return st.Index == 1 && st.Playing
	})

	p.Next()
	waitFor(t, "the second looked-up track", func() bool {
		st := p.State()
		return st.Index == 2 && st.Playing
	})

	// And back again, over an entry that now has a file.
	p.Prev()
	waitFor(t, "back to the middle", func() bool { return p.State().Index == 1 })

	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 {
		t.Fatalf("looked up %v; want both unresolved tracks", asked)
	}
	if asked[0] != "Two" {
		t.Errorf("first lookup was %q, want Two", asked[0])
	}
}

// Without a resolver the player must say so rather than stall on an entry it
// can never open.
func TestUnresolvableEntryReportsInsteadOfStalling(t *testing.T) {
	p := New(&frozenSink{})
	events, stop := p.Subscribe()
	defer stop()

	p.Play([]store.Track{{Title: "Nowhere", Artist: "A"}}, 0)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Type == "error" {
				return // reported, as it should be
			}
		case <-deadline:
			t.Fatal("no error reported for a track with no file and no resolver")
		}
	}
}

// An expiring link is never replayed. Coming back to an external track asks the
// source for a new location, because the one it gave earlier may be dead — the
// failure people see is "Error opening input file" on a URL that worked a
// minute ago.
func TestExpiringLocationsAreResolvedAgainOnReturn(t *testing.T) {
	real := makeQueue(t, 2, 400)

	var mu sync.Mutex
	lookups := 0
	p := New(&frozenSink{})
	p.Resolve = func(_ context.Context, tr store.Track) (string, error) {
		mu.Lock()
		lookups++
		mu.Unlock()
		return real[1].Path, nil
	}
	p.Play([]store.Track{
		{ID: 1, Path: real[0].Path, Title: "Local"},
		{Title: "Remote", Artist: "A", Ephemeral: true},
	}, 0)
	waitFor(t, "the local track", func() bool { return p.State().Index == 0 })

	p.Next()
	waitFor(t, "the remote track", func() bool {
		st := p.State()
		return st.Index == 1 && st.Playing
	})
	mu.Lock()
	first := lookups
	mu.Unlock()
	if first != 1 {
		t.Fatalf("%d lookups reaching the track, want 1", first)
	}

	// Away and back: the old link is not reused.
	p.Prev()
	waitFor(t, "back to the local track", func() bool { return p.State().Index == 0 })
	p.Next()
	waitFor(t, "the remote track again", func() bool {
		st := p.State()
		return st.Index == 1 && st.Playing
	})

	mu.Lock()
	defer mu.Unlock()
	if lookups != 2 {
		t.Errorf("%d lookups after returning, want 2 — the expired link was replayed", lookups)
	}
}
