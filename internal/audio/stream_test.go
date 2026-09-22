package audio

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsStream(t *testing.T) {
	for _, s := range []string{"http://example/x.mp3", "https://example/x"} {
		if !IsStream(s) {
			t.Errorf("IsStream(%q) = false", s)
		}
	}
	for _, s := range []string{"/music/a.mp3", "a.flac", "", "ftp://example/x"} {
		if IsStream(s) {
			t.Errorf("IsStream(%q) = true", s)
		}
	}
}

// The decoder runs on the engine goroutine, so reads must come from memory the
// prefetcher already filled — never straight from a socket that might stall.
func TestPrefetchServesFromMemoryWhileTheSourceIsSlow(t *testing.T) {
	slow := &slowReader{chunks: [][]byte{[]byte("abcdefghij"), []byte("klmnop")}, delay: 80 * time.Millisecond}
	p := newPrefetch(slow, func() {})
	defer p.Close()

	if err := p.waitForData(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	// By now the first chunk is buffered; reading it must not wait on the source.
	start := time.Now()
	buf := make([]byte, 10)
	n, err := p.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("read = %d, %v", n, err)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("a buffered read took %s; it went to the source", elapsed)
	}
}

func TestPrefetchReportsTheSourcesError(t *testing.T) {
	p := newPrefetch(io.NopCloser(strings.NewReader("")), func() {})
	defer p.Close()

	buf := make([]byte, 4)
	if _, err := p.Read(buf); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF once the source is exhausted", err)
	}
}

func TestCloseStopsThePrefetcher(t *testing.T) {
	endless := &endlessReader{}
	p := newPrefetch(endless, func() {})
	if err := p.waitForData(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing must release a reader waiting for data rather than hang.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := p.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a reader was left blocked after Close")
	}
}

func TestOpenStreamRejectsANonOKResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := Open(srv.URL); err == nil {
		t.Fatal("a 404 stream opened successfully")
	}
}

type slowReader struct {
	mu     sync.Mutex
	chunks [][]byte
	delay  time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.chunks[0])
	s.chunks = s.chunks[1:]
	return n, nil
}
func (s *slowReader) Close() error { return nil }

type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func (endlessReader) Close() error { return nil }
