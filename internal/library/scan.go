// Package library walks a music directory, reads tags and writes tracks to the store.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/dhowden/tag"
	"github.com/plectra/plectra/internal/audio"
	"github.com/plectra/plectra/internal/store"
)

var supported = map[string]bool{".flac": true, ".mp3": true, ".ogg": true, ".wav": true}

type Scanner struct {
	st     *store.Store
	root   string
	covers coverCache
}

// New builds a scanner. coverDir may be empty, which turns artwork extraction off.
func New(st *store.Store, root, coverDir string) *Scanner {
	return &Scanner{st: st, root: root, covers: coverCache{dir: coverDir}}
}

type Result struct {
	Scanned int
	Skipped int
	Failed  int
}

// Scan walks the root and upserts every supported file. Files whose mtime is
// unchanged are skipped, so a rescan of a large library is cheap.
func (s *Scanner) Scan(ctx context.Context) (Result, error) {
	paths := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var res Result

	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				switch err := s.scanFile(ctx, p); {
				case err == errUnchanged:
					mu.Lock()
					res.Skipped++
					mu.Unlock()
				case err != nil:
					log.Printf("scan %s: %v", p, err)
					mu.Lock()
					res.Failed++
					mu.Unlock()
				default:
					mu.Lock()
					res.Scanned++
					mu.Unlock()
				}
			}
		}()
	}

	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("walk %s: %v", p, err)
			return nil // a bad directory must not abort the scan
		}
		if d.IsDir() || !supported[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		select {
		case paths <- p:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	close(paths)
	wg.Wait()
	return res, err
}

var errUnchanged = fmt.Errorf("unchanged")

func (s *Scanner) scanFile(ctx context.Context, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if m, ok := s.st.KnownMTime(ctx, path); ok && m == fi.ModTime().Unix() {
		return errUnchanged
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	hash, err := quickHash(f, fi.Size(), fi.ModTime().Unix())
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	t := store.Track{
		Path:     path,
		FileHash: hash,
		MTime:    fi.ModTime().Unix(),
		Format:   strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
		Title:    strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Artist:   "Unknown Artist",
		Album:    "Unknown Album",
		DiscNo:   1,
	}
	if md, err := tag.ReadFrom(f); err == nil {
		if v := strings.TrimSpace(md.Title()); v != "" {
			t.Title = v
		}
		if v := strings.TrimSpace(md.Artist()); v != "" {
			t.Artist = v
		}
		if v := strings.TrimSpace(md.Album()); v != "" {
			t.Album = v
		}
		t.Year = md.Year()
		t.TrackNo, _ = md.Track()
		if d, _ := md.Disc(); d > 0 {
			t.DiscNo = d
		}
	}
	// Duration comes from container metadata, so the progress bar and the
	// completed-play threshold have real numbers from the first scan.
	// A file we cannot probe is still worth keeping: it just has no duration yet.
	if info, perr := audio.Probe(path); perr == nil {
		t.DurationMS = info.DurationMS
		t.SampleRate = info.SampleRate
		t.Channels = info.Channels
	}

	id, err := s.st.UpsertTrack(ctx, t)
	if err != nil {
		return err
	}
	s.extractCover(ctx, path, id)
	return nil
}

// quickHash identifies a file by size, mtime and its first 64KB — cheap enough to
// run on every file, stable enough that a move or rename keeps the same identity.
func quickHash(r io.Reader, size, mtime int64) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d:", size, mtime)
	if _, err := io.CopyN(h, r, 64*1024); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
