package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/hajimehoshi/go-mp3"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

// Format describes a PCM stream.
type Format struct {
	SampleRate int
	Channels   int
}

// Decoder yields interleaved float32 frames in its own native format.
type Decoder interface {
	// Read fills p with up to len(p) samples, returning io.EOF when the track ends.
	Read(p []float32) (int, error)
	Format() Format
	Close() error
}

// Open picks a decoder for a track's location: a file on disk, or a network
// stream when the location is a URL.
func Open(path string) (Decoder, error) {
	if IsStream(path) {
		return openStream(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		d, err := mp3.NewDecoder(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &pcm16Decoder{r: d, c: f, fmt: Format{d.SampleRate(), 2}, total: d.Length()}, nil
	case ".ogg":
		d, err := oggvorbis.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &vorbisDecoder{d: d, c: f}, nil
	case ".flac":
		f.Close()
		return openFLAC(path)
	case ".wav":
		return openWAV(f)
	}
	f.Close()
	// ponytail: AAC/ALAC/WMA are the ffmpeg-if-present path in a later release.
	return nil, fmt.Errorf("unsupported format: %s", filepath.Ext(path))
}

// pcm16Decoder converts a little-endian signed 16-bit stereo reader to float32.
type pcm16Decoder struct {
	r     io.Reader
	c     io.Closer
	fmt   Format
	raw   []byte
	total int64 // bytes of PCM in the stream, 0 when unknown
}

func (d *pcm16Decoder) Format() Format { return d.fmt }
func (d *pcm16Decoder) Close() error   { return d.c.Close() }

func (d *pcm16Decoder) Read(p []float32) (int, error) {
	need := len(p) * 2
	if cap(d.raw) < need {
		d.raw = make([]byte, need)
	}
	n, err := io.ReadFull(d.r, d.raw[:need])
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	n -= n % 2
	for i := 0; i < n/2; i++ {
		p[i] = float32(int16(binary.LittleEndian.Uint16(d.raw[i*2:]))) / 32768
	}
	if n == 0 && err == nil {
		err = io.EOF
	}
	return n / 2, err
}

type vorbisDecoder struct {
	d *oggvorbis.Reader
	c io.Closer
}

func (d *vorbisDecoder) Format() Format { return Format{d.d.SampleRate(), d.d.Channels()} }
func (d *vorbisDecoder) Close() error   { return d.c.Close() }
func (d *vorbisDecoder) Read(p []float32) (int, error) {
	return d.d.Read(p)
}

type flacDecoder struct {
	s     *flac.Stream
	fmt   Format
	buf   []float32
	scale float32
}

func openFLAC(path string) (Decoder, error) {
	s, err := flac.ParseFile(path)
	if err != nil {
		return nil, err
	}
	return &flacDecoder{
		s:     s,
		fmt:   Format{int(s.Info.SampleRate), int(s.Info.NChannels)},
		scale: float32(int32(1) << (s.Info.BitsPerSample - 1)),
	}, nil
}

func (d *flacDecoder) Format() Format { return d.fmt }
func (d *flacDecoder) Close() error   { return d.s.Close() }

func (d *flacDecoder) Read(p []float32) (int, error) {
	for len(d.buf) == 0 {
		fr, err := d.s.ParseNext()
		if err != nil {
			return 0, err // io.EOF at end of stream
		}
		n := len(fr.Subframes[0].Samples)
		d.buf = make([]float32, 0, n*len(fr.Subframes))
		for i := 0; i < n; i++ {
			for _, sf := range fr.Subframes {
				d.buf = append(d.buf, float32(sf.Samples[i])/d.scale)
			}
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

// openWAV handles uncompressed PCM only, which is what WAV is in practice.
func openWAV(f *os.File) (Decoder, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		f.Close()
		return nil, err
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		f.Close()
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	var format Format
	var bits int
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			f.Close()
			return nil, err
		}
		id, size := string(ch[0:4]), int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			b := make([]byte, size)
			if _, err := io.ReadFull(f, b); err != nil {
				f.Close()
				return nil, err
			}
			format = Format{int(binary.LittleEndian.Uint32(b[4:8])), int(binary.LittleEndian.Uint16(b[2:4]))}
			bits = int(binary.LittleEndian.Uint16(b[14:16]))
		case "data":
			if bits != 16 {
				f.Close()
				return nil, fmt.Errorf("wav: only 16-bit PCM supported, got %d", bits)
			}
			return &pcm16Decoder{r: io.LimitReader(f, size), c: f, fmt: format, total: size}, nil
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				f.Close()
				return nil, err
			}
		}
	}
}

// resample converts src (frames interleaved at from.Channels) to the sink format by
// linear interpolation and channel duplication.
// ponytail: linear interpolation is audibly fine for the common 44.1k->48k case.
// Swap in a windowed-sinc resampler if anyone complains about high-frequency artefacts.
func Resample(src []float32, from, to Format) []float32 {
	if from == to {
		return src
	}
	inFrames := len(src) / from.Channels
	ratio := float64(to.SampleRate) / float64(from.SampleRate)
	outFrames := int(float64(inFrames) * ratio)
	out := make([]float32, outFrames*to.Channels)
	for i := 0; i < outFrames; i++ {
		pos := float64(i) / ratio
		i0 := int(pos)
		frac := float32(pos - math.Floor(pos))
		i1 := i0 + 1
		if i1 >= inFrames {
			i1 = inFrames - 1
		}
		for c := 0; c < to.Channels; c++ {
			sc := c % from.Channels
			a := src[i0*from.Channels+sc]
			b := src[i1*from.Channels+sc]
			out[i*to.Channels+c] = a + (b-a)*frac
		}
	}
	return out
}

// Info is what a scanner needs to know about a file without playing it.
type Info struct {
	Format
	DurationMS int64
}

// Probe reads duration and format from container metadata where the format
// allows it. MP3 has no length field, so its frames are counted — still far
// cheaper than decoding, and it is the only format that needs the walk.
func Probe(path string) (Info, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		s, err := flac.ParseFile(path)
		if err != nil {
			return Info{}, err
		}
		defer s.Close()
		f := Format{int(s.Info.SampleRate), int(s.Info.NChannels)}
		return Info{f, int64(s.Info.NSamples) * 1000 / int64(f.SampleRate)}, nil

	case ".ogg":
		f, err := os.Open(path)
		if err != nil {
			return Info{}, err
		}
		defer f.Close()
		r, err := oggvorbis.NewReader(f)
		if err != nil {
			return Info{}, err
		}
		fm := Format{r.SampleRate(), r.Channels()}
		return Info{fm, r.Length() * 1000 / int64(fm.SampleRate)}, nil

	case ".wav", ".mp3":
		d, err := Open(path)
		if err != nil {
			return Info{}, err
		}
		defer d.Close()
		fm := d.Format()
		if p, ok := d.(*pcm16Decoder); ok && p.total > 0 {
			return Info{fm, p.total * 1000 / int64(fm.SampleRate*fm.Channels) / 2}, nil
		}
		return Info{fm, 0}, nil
	}
	return Info{}, fmt.Errorf("cannot probe %s", filepath.Ext(path))
}
