package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/plectra/plectra/internal/audio"
)

// YTDLP finds audio through yt-dlp, which the user installs themselves. It is
// absent unless both yt-dlp and ffmpeg are on PATH: yt-dlp locates the stream,
// ffmpeg decodes formats Plectra cannot read on its own.
//
// Nothing is downloaded and nothing is cached: a URL is resolved when a track
// is about to play and is thrown away afterwards, because it expires anyway.
type YTDLP struct {
	// Binary is the command to run; overridable for tests.
	Binary string
	// Timeout bounds each invocation. yt-dlp can hang on a slow network.
	Timeout time.Duration
}

func NewYTDLP() *YTDLP {
	return &YTDLP{Binary: "yt-dlp", Timeout: 45 * time.Second}
}

func (y *YTDLP) Name() string { return "yt-dlp" }

func (y *YTDLP) Available() bool {
	if _, err := exec.LookPath(y.Binary); err != nil {
		return false
	}
	// Without ffmpeg the formats it returns cannot be decoded, so offering the
	// provider would mean offering tracks that fail at play time.
	return audio.HasFFmpeg()
}

// Find searches and returns candidates without resolving any stream URL.
func (y *YTDLP) Find(ctx context.Context, q Query) ([]Candidate, error) {
	query := q.String()
	if query == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, y.Timeout)
	defer cancel()

	// --flat-playlist keeps this to one request per result instead of one per
	// video, which is the difference between a search and a crawl.
	out, err := exec.CommandContext(ctx, y.Binary,
		"--no-warnings", "--quiet",
		"--flat-playlist",
		"--dump-json",
		"--playlist-end", "5",
		"ytsearch5:"+query,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", trimStderr(err))
	}

	var found []Candidate
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var item struct {
			ID       string  `json:"id"`
			Title    string  `json:"title"`
			Uploader string  `json:"uploader"`
			Channel  string  `json:"channel"`
			Duration float64 `json:"duration"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		artist := item.Uploader
		if artist == "" {
			artist = item.Channel
		}
		found = append(found, Candidate{
			ID:         item.ID,
			Provider:   y.Name(),
			Title:      item.Title,
			Artist:     artist,
			DurationMS: int64(item.Duration * 1000),
		})
	}
	return found, nil
}

// Resolve asks for a direct audio URL, which Plectra hands to ffmpeg.
func (y *YTDLP) Resolve(ctx context.Context, c Candidate) (string, error) {
	if c.ID == "" {
		return "", fmt.Errorf("candidate has no id")
	}
	ctx, cancel := context.WithTimeout(ctx, y.Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, y.Binary,
		"--no-warnings", "--quiet",
		"--no-playlist",
		"-f", "bestaudio",
		"-g",
		"https://www.youtube.com/watch?v="+c.ID,
	).Output()
	if err != nil {
		return "", fmt.Errorf("resolve failed: %w", trimStderr(err))
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("no audio url returned")
	}
	if i := strings.IndexByte(url, '\n'); i > 0 {
		url = url[:i]
	}
	// The formats served here are Opus and AAC, which the pure-Go decoders
	// cannot read; ffmpeg does that part.
	return audio.FFmpegScheme + url, nil
}

// trimStderr surfaces what the tool actually said instead of "exit status 1".
func trimStderr(err error) error {
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok && len(ee.Stderr) > 0 {
		msg := strings.TrimSpace(string(ee.Stderr))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("%s", msg)
	}
	return err
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
