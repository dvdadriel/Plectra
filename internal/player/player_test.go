package player

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
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
