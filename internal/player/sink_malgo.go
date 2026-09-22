package player

import (
	"github.com/plectra/plectra/internal/audio"

	"sync/atomic"

	"github.com/gen2brain/malgo"
)

// malgoSink plays to the default output device via miniaudio.
type malgoSink struct {
	ctx    *malgo.AllocatedContext
	dev    *malgo.Device
	buf    *ring
	fmt    audio.Format
	played atomic.Int64
	silent atomic.Int64 // underrun frames, a metric worth watching
}

// NewDeviceSink opens the default output device at 48kHz stereo.
func NewDeviceSink() (Sink, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	s := &malgoSink{
		ctx: ctx,
		fmt: audio.Format{SampleRate: 48000, Channels: 2},
	}
	s.buf = newRing(s.fmt.SampleRate * s.fmt.Channels / 2) // ~500ms

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatF32
	cfg.Playback.Channels = uint32(s.fmt.Channels)
	cfg.SampleRate = uint32(s.fmt.SampleRate)

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: s.callback})
	if err != nil {
		ctx.Uninit()
		return nil, err
	}
	if err := dev.Start(); err != nil {
		ctx.Uninit()
		return nil, err
	}
	s.dev = dev
	return s, nil
}

// callback runs on the OS audio thread. It allocates nothing, takes no lock,
// touches no disk and never blocks — it copies frames and bumps counters.
func (s *malgoSink) callback(out, _ []byte, frames uint32) {
	pcm := f32slice(out, int(frames)*s.fmt.Channels)
	n := s.buf.read(pcm)
	for i := n; i < len(pcm); i++ {
		pcm[i] = 0 // underrun: silence, never a repeat of stale audio
	}
	if n < len(pcm) {
		s.silent.Add(int64((len(pcm) - n) / s.fmt.Channels))
	}
	s.played.Add(int64(n / s.fmt.Channels))
}

func (s *malgoSink) Write(pcm []float32) (int, error) { return s.buf.write(pcm), nil }
func (s *malgoSink) Format() audio.Format             { return s.fmt }
func (s *malgoSink) Played() int64                    { return s.played.Load() }

func (s *malgoSink) Close() error {
	if s.dev != nil {
		s.dev.Uninit()
	}
	return s.ctx.Uninit()
}
