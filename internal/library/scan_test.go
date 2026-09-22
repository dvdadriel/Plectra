package library

import (
	"context"
	"encoding/binary"
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
