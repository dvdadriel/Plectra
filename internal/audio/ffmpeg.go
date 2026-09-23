package audio

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// FFmpegScheme marks a location that must be decoded by ffmpeg rather than by
// the pure-Go decoders: formats we cannot read ourselves, such as the Opus and
// AAC streams YouTube serves.
const FFmpegScheme = "ffmpeg:"

// streamUserAgent matches what yt-dlp sends when it resolves a URL. Some hosts
// hand out links bound to the requesting client and refuse anything else.
const streamUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// HasFFmpeg reports whether ffmpeg is on PATH. Everything that depends on it is
// optional: without ffmpeg those sources simply do not appear.
func HasFFmpeg() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// openFFmpeg decodes anything ffmpeg can read into the one format the player
// already understands: signed 16-bit stereo PCM at 48kHz. ffmpeg does the
// network fetch, the demuxing and the decoding; Plectra just reads samples.
func openFFmpeg(location string) (Decoder, error) {
	url := strings.TrimPrefix(location, FFmpegScheme)
	if !HasFFmpeg() {
		return nil, fmt.Errorf("this source needs ffmpeg, which is not installed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "ffmpeg",
		// warning, not error: at error level ffmpeg swallows the HTTP status,
		// so a refused stream reported only "Error opening input file" and the
		// URL, which says nothing about why.
		"-loglevel", "warning",
		// The media hosts these URLs come from serve them to the client that
		// asked. ffmpeg's default agent is not that client.
		"-user_agent", streamUserAgent,
		"-reconnect", "1", // a dropped connection mid-track is recoverable
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-rw_timeout", "15000000", // 15s: a hung socket must not hang playback
		"-i", url,
		"-vn",
		"-f", "s16le",
		"-ar", "48000",
		"-ac", "2",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	// The same prefetcher the radio streams use: the decoder runs on the engine
	// goroutine and must never wait on a pipe.
	buf := newPrefetch(&processReader{r: stdout, cmd: cmd, cancel: cancel, stderr: &stderr}, cancel)
	if err := buf.waitForData(20 * time.Second); err != nil {
		buf.Close()
		return nil, fmt.Errorf("ffmpeg produced nothing: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return &pcm16Decoder{r: buf, c: buf, fmt: Format{SampleRate: 48000, Channels: 2}}, nil
}

// processReader ties the pipe's lifetime to the process, so closing playback
// also stops ffmpeg instead of leaving it running.
type processReader struct {
	r      io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *strings.Builder

	once sync.Once
}

func (p *processReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *processReader) Close() error {
	p.once.Do(func() {
		p.cancel()
		p.r.Close()
		p.cmd.Wait() // reap it; the context has already asked it to stop
	})
	return nil
}
