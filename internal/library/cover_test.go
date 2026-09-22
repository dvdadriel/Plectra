package library

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

// id3WithPicture builds an ID3v2.3 tag carrying one PNG APIC frame. The audio
// after it is irrelevant here: the scanner must survive a file it cannot decode
// and still keep the artwork.
func id3WithPicture(png []byte) []byte {
	var apic []byte
	apic = append(apic, 0)              // text encoding: ISO-8859-1
	apic = append(apic, "image/png"...) //
	apic = append(apic, 0)              // MIME terminator
	apic = append(apic, 3)              // picture type: front cover
	apic = append(apic, 0)              // empty description
	apic = append(apic, png...)         //

	frame := append([]byte("APIC"), 0, 0, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(apic)))
	frame = append(frame, apic...)

	tag := append([]byte("ID3"), 3, 0, 0, 0, 0, 0, 0)
	// ID3v2 sizes are seven bits per byte.
	n := len(frame)
	for i := 0; i < 4; i++ {
		tag[9-i] = byte(n>>(7*i)) & 0x7f
	}
	return append(tag, frame...)
}

func TestCoverArtIsExtractedOncePerAlbum(t *testing.T) {
	dir, cache := t.TempDir(), filepath.Join(t.TempDir(), "covers")
	png := []byte("\x89PNG\r\n\x1a\n" + "fake image bytes")
	if err := os.WriteFile(filepath.Join(dir, "song.mp3"), id3WithPicture(png), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := New(st, dir, cache).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	albums, err := st.Albums(ctx)
	if err != nil || len(albums) != 1 {
		t.Fatalf("albums = %v, err = %v", albums, err)
	}
	path, err := st.AlbumCover(ctx, albums[0].ID)
	if err != nil {
		t.Fatalf("AlbumCover: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(png) {
		t.Fatalf("cached artwork = %q, err = %v", got, err)
	}
	if filepath.Ext(path) != ".png" {
		t.Fatalf("cached as %s, want .png", path)
	}
}

func TestAlbumWithoutArtworkHasNoCover(t *testing.T) {
	dir := t.TempDir()
	writeWAV(t, filepath.Join(dir, "plain.wav"), 200)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := New(st, dir, filepath.Join(t.TempDir(), "covers")).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	albums, _ := st.Albums(ctx)
	if _, err := st.AlbumCover(ctx, albums[0].ID); err != store.ErrNotFound {
		t.Fatalf("AlbumCover = %v, want ErrNotFound", err)
	}
}
