package library

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"

	"github.com/dhowden/tag"
)

// CoverDir is where extracted artwork is cached; set by the caller at wiring time.
type coverCache struct{ dir string }

// extractCover pulls embedded artwork out of a file and caches it on disk, once
// per album. Artwork failures are logged and forgotten — they never fail a scan.
func (s *Scanner) extractCover(ctx context.Context, path string, trackID int64) {
	if s.covers.dir == "" {
		return
	}
	albumID, has, err := s.st.AlbumCoverState(ctx, trackID)
	if err != nil || has {
		return // already have one; the first file of an album wins
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	md, err := tag.ReadFrom(f)
	if err != nil {
		return
	}
	pic := md.Picture()
	if pic == nil || len(pic.Data) == 0 {
		return
	}

	ext := ".jpg"
	if pic.MIMEType == "image/png" || pic.Ext == "png" {
		ext = ".png"
	}
	// Name by content hash: re-scanning the same art rewrites the same file.
	sum := sha1.Sum(pic.Data)
	name := hex.EncodeToString(sum[:]) + ext
	dst := filepath.Join(s.covers.dir, name)

	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(s.covers.dir, 0o755); err != nil {
			log.Printf("cover cache: %v", err)
			return
		}
		if err := os.WriteFile(dst, pic.Data, 0o644); err != nil {
			log.Printf("cover write: %v", err)
			return
		}
	}
	if err := s.st.SetAlbumCover(ctx, albumID, dst); err != nil {
		log.Printf("cover path: %v", err)
	}
}
