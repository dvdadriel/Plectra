package player

import "github.com/plectra/plectra/internal/audio"

// Sink is where audio leaves the process. v1: the local sound card.
type Sink interface {
	// Write hands interleaved float32 frames to the device. It never blocks:
	// it returns how many samples it accepted, which may be fewer than offered.
	Write(pcm []float32) (int, error)
	Format() audio.Format
	// Played reports frames the device has actually consumed. This, not the wall
	// clock, is the source of truth for playback position.
	Played() int64
	Close() error
}
