package audio

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
)

// IsStream reports whether a track's location is a network stream rather than a
// file on disk.
func IsStream(location string) bool {
	return strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://")
}

// openStream plays a network stream — internet radio. The decoder runs on the
// engine goroutine, so a read that waits on the network would freeze playback
// controls as well as the music. A goroutine therefore pulls from the socket
// into a buffer ahead of the decoder, which then reads from memory.
func openStream(url string) (Decoder, error) {
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("User-Agent", "Plectra/0.7")
	// Ask for no ICY metadata: the titles are interesting, but mixing them into
	// the audio bytes means parsing them correctly or corrupting the sound.
	req.Header.Set("Icy-MetaData", "0")

	// No client timeout: a radio stream is meant to never end. The read
	// deadline below is what catches a dead connection.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("stream: status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	buf := newPrefetch(resp.Body, cancel)

	switch {
	case strings.Contains(ct, "ogg"):
		return newVorbisStream(buf)
	default:
		// Everything else is treated as MP3, which is what shoutcast/icecast
		// stations overwhelmingly are. A station that is something else fails
		// here rather than playing noise.
		return newMP3Stream(buf)
	}
}

// prefetch reads ahead of the decoder so a network hiccup costs buffered audio
// rather than a frozen player.
type prefetch struct {
	src    io.ReadCloser
	cancel context.CancelFunc

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	err    error
	closed bool
}

// ponytail: 512KB is about 30 seconds at 128kbps — enough to ride out a stall
// without holding a stream's worth of memory. Raise it if stations stutter.
const prefetchLimit = 512 << 10

func newPrefetch(src io.ReadCloser, cancel context.CancelFunc) *prefetch {
	p := &prefetch{src: src, cancel: cancel}
	p.cond = sync.NewCond(&p.mu)
	go p.fill()
	return p
}

func (p *prefetch) fill() {
	chunk := make([]byte, 32<<10)
	for {
		p.mu.Lock()
		for len(p.buf) > prefetchLimit && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		n, err := p.src.Read(chunk)
		p.mu.Lock()
		if n > 0 {
			p.buf = append(p.buf, chunk[:n]...)
		}
		if err != nil {
			p.err = err
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		p.cond.Broadcast()
		p.mu.Unlock()
	}
}

func (p *prefetch) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 {
		if p.closed {
			return 0, io.EOF
		}
		if p.err != nil {
			return 0, p.err
		}
		p.cond.Wait()
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	p.cond.Broadcast()
	return n, nil
}

func (p *prefetch) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()

	p.cancel()
	return p.src.Close()
}

// waitForData blocks until the prefetcher has something, so a decoder that
// probes the first bytes on construction does not fail on an empty buffer.
func (p *prefetch) waitForData(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		p.mu.Lock()
		have, err := len(p.buf), p.err
		p.mu.Unlock()
		if have > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stream: no data within %s", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newMP3Stream decodes a shoutcast/icecast MP3 stream.
func newMP3Stream(p *prefetch) (Decoder, error) {
	if err := p.waitForData(10 * time.Second); err != nil {
		p.Close()
		return nil, err
	}
	d, err := mp3.NewDecoder(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return &pcm16Decoder{r: d, c: p, fmt: Format{d.SampleRate(), 2}}, nil
}

// newVorbisStream decodes an Ogg Vorbis stream.
func newVorbisStream(p *prefetch) (Decoder, error) {
	if err := p.waitForData(10 * time.Second); err != nil {
		p.Close()
		return nil, err
	}
	r, err := oggvorbis.NewReader(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return &vorbisDecoder{d: r, c: p}, nil
}
