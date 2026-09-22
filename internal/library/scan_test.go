package library

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

func TestScanIsIdempotentAndSurvivesJunk(t *testing.T) {
	dir := t.TempDir()
	// A file with no readable tags still has to land in the library.
	if err := os.WriteFile(filepath.Join(dir, "untagged.wav"), []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file we do not support is ignored, not failed.
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	s := New(st, dir, filepath.Join(t.TempDir(), "covers"))

	r, err := s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Scanned != 1 || r.Failed != 0 {
		t.Fatalf("first scan: %+v", r)
	}

	r, err = s.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 1 || r.Scanned != 0 {
		t.Fatalf("rescan should skip unchanged files: %+v", r)
	}

	albums, err := st.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %v, err = %v", albums, err)
	}
}

// writeWAV writes a tiny valid 16-bit stereo WAV so probing has something real.
func writeWAV(t *testing.T, path string, ms int) {
	t.Helper()
	const rate, ch = 44100, 2
	data := make([]byte, rate*ms/1000*ch*2)
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

func TestScanFillsDuration(t *testing.T) {
	dir := t.TempDir()
	writeWAV(t, filepath.Join(dir, "song.wav"), 1500)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := New(st, dir, "").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	albums, _ := st.Albums(ctx)
	tracks, err := st.TracksByAlbum(ctx, albums[0].ID)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks = %v, err = %v", tracks, err)
	}
	if d := tracks[0].DurationMS; d < 1450 || d > 1550 {
		t.Fatalf("duration = %dms, want about 1500", d)
	}
}

func TestWatcherPicksUpNewFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(st, dir, "")

	changed := make(chan struct{}, 4)
	go s.Watch(ctx, func() { changed <- struct{}{} })
	time.Sleep(200 * time.Millisecond) // let the watcher register the directory

	writeWAV(t, filepath.Join(dir, "new.wav"), 300)

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never reported a change")
	}
	albums, err := st.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %v, err = %v", albums, err)
	}
}

// Two different files can share a size, an mtime and their first 64KB — audio
// exported in one session, or tracks with the same long intro. They must stay
// two tracks.
func TestFilesWithIdenticalHeadsStayDistinct(t *testing.T) {
	for _, size := range []int{4 * 1024, 200 * 1024} { // below and above the hash windows
		t.Run(fmt.Sprintf("%dKB", size/1024), func(t *testing.T) {
			assertDistinct(t, size)
		})
	}
}

func assertDistinct(t *testing.T, long int) {
	t.Helper()
	dir := t.TempDir()

	head := make([]byte, long)
	for i := range head {
		head[i] = byte(i % 251)
	}
	for i, name := range []string{"one.wav", "two.wav"} {
		body := append([]byte(nil), head...)
		body[len(body)-1] = byte(i) // differ only in the final byte
		writeWAVBody(t, filepath.Join(dir, name), body)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := New(st, dir, "").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	albums, _ := st.Albums(ctx)
	tracks, err := st.TracksByAlbum(ctx, albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("scanned %d tracks, want 2 — one file swallowed the other", len(tracks))
	}
}

// writeWAVBody writes a 16-bit stereo WAV carrying exactly the given PCM bytes.
func writeWAVBody(t *testing.T, path string, data []byte) {
	t.Helper()
	const rate, ch = 44100, 2
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

func TestYearFromRealTagShapes(t *testing.T) {
	now := time.Now().Year()
	cases := []struct {
		name string
		year int
		raw  map[string]any
		want int
	}{
		{"plain year", 1998, nil, 1998},
		// What a real Vorbis file carries: a full timestamp the tag library
		// reduces to 1.
		{"timestamp date", 1, map[string]any{"date": "2014-11-19T16:15:12"}, 2014},
		{"date only", 0, map[string]any{"date": "1973-03-01"}, 1973},
		{"id3 frame", 0, map[string]any{"TDRC": "1969"}, 1969},
		{"garbage", 1, map[string]any{"date": "not a date"}, 0},
		{"absent", 0, nil, 0},
		{"absurd future", now + 50, nil, 0},
	}
	for _, c := range cases {
		if got := yearFrom(c.year, c.raw); got != c.want {
			t.Errorf("%s: yearFrom(%d, %v) = %d, want %d", c.name, c.year, c.raw, got, c.want)
		}
	}
}

func TestStartScanReportsStatusAndRefusesToOverlap(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.wav", "b.wav"} {
		writeWAV(t, filepath.Join(dir, name), 200+len(name)*10)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := New(st, dir, "")
	ctx := context.Background()

	// The root is known before anything has been scanned.
	if got := s.Status(); got.Root != dir || got.Running {
		t.Fatalf("status before scanning = %+v", got)
	}

	if !s.StartScan(ctx) {
		t.Fatal("the first scan did not start")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.Status(); !got.Running && got.FinishedAt != 0 {
			if got.Scanned != 2 {
				t.Fatalf("scanned %d files, want 2 (%+v)", got.Scanned, got)
			}
			// A second scan is allowed once the first has finished.
			if !s.StartScan(ctx) {
				t.Fatal("a scan would not start after the previous one finished")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("scan never finished: %+v", s.Status())
}

// Pressing the button twice must not start two walks over the same tree.
func TestStartScanIsRefusedWhileOneIsRunning(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 40; i++ {
		writeWAV(t, filepath.Join(dir, fmt.Sprintf("t%02d.wav", i)), 400)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := New(st, dir, "")
	if !s.StartScan(context.Background()) {
		t.Fatal("the first scan did not start")
	}
	// While that one is walking, a second request is refused rather than queued.
	refusedWhileRunning := false
	for i := 0; i < 200; i++ {
		if s.Status().Running {
			refusedWhileRunning = !s.StartScan(context.Background())
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !refusedWhileRunning {
		t.Fatal("a second scan started while the first was still running")
	}
}
