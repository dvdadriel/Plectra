// Package player owns the audio engine. It knows nothing of HTTP.
package player

import (
	"io"
	"log"
	"math/rand"

	"github.com/plectra/plectra/internal/audio"
	"sync"
	"time"

	"github.com/plectra/plectra/internal/store"
)

type Event struct {
	Type    string `json:"type"` // state | track | error | position
	State   *State `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
}

type State struct {
	Playing    bool          `json:"playing"`
	Index      int           `json:"index"`
	PositionMS int64         `json:"positionMs"`
	Volume     float64       `json:"volume"`
	Shuffle    bool          `json:"shuffle"`
	Repeat     string        `json:"repeat"` // off | all | one
	Queue      []store.Track `json:"queue"`
}

type command struct {
	kind  string
	queue []store.Track
	index int
	ms    int64
	f     float64
	s     string
	b     bool
}

// Player runs one goroutine that owns all playback state. Control comes in through
// cmds; state goes out through subscribers. No mutex on the playback path.
type Player struct {
	cmds   chan command
	states chan chan State
	sink   Sink

	subMu sync.Mutex
	subs  map[chan Event]struct{}

	// owned by run()
	queue      []store.Track
	index      int
	playing    bool
	volume     float64
	shuffle    bool
	repeat     string
	dec        audio.Decoder
	pending    []float32 // resampled audio the sink had no room for yet
	pushed     int64     // frames handed to the sink since start
	trackStart int64     // sink frame at which the current track began
	trackOff   int64     // ms skipped into the current track by a seek or a restore
	drainTo    int64     // stop only once the sink has consumed this frame
	switches   []switchPoint
}

type switchPoint struct {
	atFrame int64
	index   int
}

func New(sink Sink) *Player {
	p := &Player{
		cmds:   make(chan command, 8),
		states: make(chan chan State, 4),
		sink:   sink,
		subs:   map[chan Event]struct{}{},
		volume: 1,
		repeat: "off",
		index:  -1,
	}
	go p.run()
	return p
}

func (p *Player) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	p.subMu.Lock()
	p.subs[ch] = struct{}{}
	p.subMu.Unlock()
	return ch, func() {
		p.subMu.Lock()
		delete(p.subs, ch)
		close(ch)
		p.subMu.Unlock()
	}
}

func (p *Player) emit(e Event) {
	p.subMu.Lock()
	defer p.subMu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- e:
		default: // a slow client must never stall the engine
		}
	}
}

func (p *Player) send(c command) { p.cmds <- c }
func (p *Player) Play(q []store.Track, start int) {
	p.send(command{kind: "play", queue: q, index: start})
}
func (p *Player) Pause()              { p.send(command{kind: "pause"}) }
func (p *Player) Resume()             { p.send(command{kind: "resume"}) }
func (p *Player) Next()               { p.send(command{kind: "next"}) }
func (p *Player) Prev()               { p.send(command{kind: "prev"}) }
func (p *Player) SeekMS(ms int64)     { p.send(command{kind: "seek", ms: ms}) }
func (p *Player) SetVolume(v float64) { p.send(command{kind: "volume", f: v}) }
func (p *Player) SetMode(shuffle bool, repeat string) {
	p.send(command{kind: "mode", b: shuffle, s: repeat})
}
func (p *Player) Enqueue(q []store.Track) { p.send(command{kind: "enqueue", queue: q}) }

// Restore loads a saved queue without starting playback: after a restart the
// music waits for the user, it does not ambush them.
func (p *Player) Restore(q []store.Track, index int, positionMS int64, volume float64, shuffle bool, repeat string) {
	p.send(command{kind: "restore", queue: q, index: index, ms: positionMS, f: volume, b: shuffle, s: repeat})
}

// State asks run() for a snapshot, so readers never touch engine-owned fields.
func (p *Player) State() State {
	reply := make(chan State, 1)
	p.states <- reply
	return <-reply
}

const chunkFrames = 2048

func (p *Player) run() {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	ticks := 0
	for {
		select {
		case c := <-p.cmds:
			p.handle(c)
		case reply := <-p.states:
			reply <- p.snapshot()
		case <-tick.C:
			p.pump()
			p.advanceSwitches()
			// Broadcast position twice a second; it comes from sink frames, not the clock.
			if ticks++; p.playing && ticks%50 == 0 {
				p.emitState()
			}
		}
	}
}

func (p *Player) snapshot() State {
	return State{
		Playing:    p.playing,
		Index:      p.index,
		PositionMS: p.positionMS(),
		Volume:     p.volume,
		Shuffle:    p.shuffle,
		Repeat:     p.repeat,
		Queue:      p.queue,
	}
}

func (p *Player) positionMS() int64 {
	if p.index < 0 {
		return 0
	}
	frames := p.sink.Played() - p.trackStart
	if frames < 0 {
		frames = 0
	}
	return p.trackOff + frames*1000/int64(p.sink.Format().SampleRate)
}

func (p *Player) handle(c command) {
	switch c.kind {
	case "play":
		p.queue = c.queue
		p.openAt(c.index, 0)
	case "restore":
		p.queue = c.queue
		p.volume, p.shuffle, p.repeat = clamp(c.f), c.b, c.s
		if p.repeat == "" {
			p.repeat = "off"
		}
		p.openAt(c.index, c.ms)
		p.playing = false // restored paused
	case "enqueue":
		p.queue = append(p.queue, c.queue...)
	case "pause":
		p.playing = false
	case "resume":
		if p.dec != nil {
			p.playing = true
		}
	case "next":
		p.openAt(p.index+1, 0)
	case "prev":
		if p.positionMS() > 3000 {
			p.openAt(p.index, 0)
		} else {
			p.openAt(p.index-1, 0)
		}
	case "seek":
		p.openAt(p.index, c.ms)
	case "volume":
		p.volume = clamp(c.f)
	case "mode":
		if c.b && !p.shuffle {
			p.shuffleRest()
		}
		p.shuffle, p.repeat = c.b, c.s
	}
	p.emitState()
}

func (p *Player) emitState() {
	s := p.snapshot()
	p.emit(Event{Type: "state", State: &s})
}

// openAt starts index at offsetMS, discarding whatever is buffered.
func (p *Player) openAt(index int, offsetMS int64) {
	if p.dec != nil {
		p.dec.Close()
		p.dec = nil
	}
	p.switches = nil
	p.pending = nil
	p.drainTo = 0
	p.playing = false
	if index < 0 || index >= len(p.queue) {
		p.index = -1
		return
	}
	d, err := audio.Open(p.queue[index].Path)
	if err != nil {
		log.Printf("player: open %s: %v", p.queue[index].Path, err)
		p.emit(Event{Type: "error", Message: err.Error()})
		if index+1 < len(p.queue) {
			p.openAt(index+1, 0) // one broken file must not stop the music
		}
		return
	}
	if offsetMS > 0 {
		// ponytail: seek by decoding and discarding. Exact for every format and
		// costs ~a few ms per minute skipped; add per-format seeking if that shows up.
		skip := offsetMS * int64(d.Format().SampleRate) * int64(d.Format().Channels) / 1000
		buf := make([]float32, 8192)
		for skip > 0 {
			n, err := d.Read(buf[:min64(int64(len(buf)), skip)])
			if err != nil {
				break
			}
			skip -= int64(n)
		}
	}
	p.dec = d
	p.index = index
	p.playing = true
	p.trackOff = offsetMS
	p.trackStart = p.sink.Played()
	p.pushed = p.trackStart
}

// pump decodes ahead and feeds the sink. Runs on the engine goroutine, never
// on the audio thread.
func (p *Player) pump() {
	if !p.playing {
		return
	}
	sinkFmt := p.sink.Format()

	// feed hands out to the sink, keeping whatever did not fit for the next tick.
	feed := func(out []float32) bool {
		for len(out) > 0 {
			w, err := p.sink.Write(out)
			p.pushed += int64(w / sinkFmt.Channels)
			out = out[w:]
			if err != nil {
				p.emit(Event{Type: "error", Message: err.Error()})
				p.pending = nil
				return false
			}
			if w == 0 {
				p.pending = out // sink full; retry next tick, nothing is dropped
				return false
			}
		}
		p.pending = nil
		return true
	}

	if len(p.pending) > 0 && !feed(p.pending) {
		return
	}
	if p.dec == nil {
		return
	}
	for i := 0; i < 8; i++ { // bounded so commands stay responsive
		df := p.dec.Format()
		buf := make([]float32, chunkFrames*df.Channels)
		n, err := p.dec.Read(buf)
		if n > 0 {
			out := audio.Resample(buf[:n], df, sinkFmt)
			gain := float32(p.volume)
			for i := range out {
				out[i] *= gain
			}
			if !feed(out) {
				return
			}
		}
		if err == io.EOF || (err != nil && n == 0) {
			if err != io.EOF {
				p.emit(Event{Type: "error", Message: err.Error()})
			}
			p.gapless()
			return
		}
	}
}

// gapless opens the next track and keeps writing into the same open device.
func (p *Player) gapless() {
	p.dec.Close()
	p.dec = nil

	next := p.index + 1
	if p.repeat == "one" {
		next = p.index
	} else if next >= len(p.queue) {
		if p.repeat != "all" {
			// Keep playing until the ring has actually drained; the last ~500ms
			// of audio is still queued in the sink.
			p.drainTo = p.pushed
			return
		}
		next = 0
	}
	d, err := audio.Open(p.queue[next].Path)
	if err != nil {
		p.emit(Event{Type: "error", Message: err.Error()})
		p.index = next
		p.gapless() // skip the bad file, keep going
		return
	}
	p.dec = d
	// The old track is still in the ring; the position flips to the new track
	// only once the sink has actually consumed up to this frame.
	p.switches = append(p.switches, switchPoint{atFrame: p.pushed, index: next})
}

func (p *Player) advanceSwitches() {
	played := p.sink.Played()
	if p.drainTo > 0 && played >= p.drainTo {
		p.drainTo = 0
		p.playing = false
		p.emitState()
	}
	for len(p.switches) > 0 && played >= p.switches[0].atFrame {
		p.index = p.switches[0].index
		p.trackStart = p.switches[0].atFrame
		p.trackOff = 0
		p.switches = p.switches[1:]
		p.emitState()
	}
}

// shuffleRest reorders everything after the current track, leaving what is
// playing where it is.
func (p *Player) shuffleRest() {
	rest := p.queue[p.index+1:]
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
