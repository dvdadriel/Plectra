// Package player owns the audio engine. It knows nothing of HTTP.
package player

import (
	"context"
	"io"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plectra/plectra/internal/audio"
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
	// reply carries a state snapshot back. A read travels the same channel as
	// the writes so it is answered after them: two channels and a select would
	// let State() overtake the Play() that was sent first.
	reply chan State
}

// Player runs one goroutine that owns all playback state. Control comes in through
// cmds; state goes out through subscribers. No mutex on the playback path.
type Player struct {
	cmds chan command
	sink Sink

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
	pausedAt   int64     // ms into the track when pause discarded the buffer
	switches   []switchPoint

	// Suggest, when set, is asked for more music as the queue runs down, so
	// playback continues with no browser open. Nil by default: the player works
	// without it and this package imports nothing to provide it.
	//
	// It is called on its own goroutine — never on the pump loop, which is
	// feeding the audio device. A database query or an HTTP call there is a
	// dropout you can hear.
	Suggest   func(ctx context.Context, seed store.Track) []store.Track
	refilling atomic.Bool

	// Resolve turns a queue entry that has no file into a playable location.
	// Tracks from a catalogue album arrive with a name and an artist and
	// nothing else; looking one up costs seconds, so it happens when the track
	// comes up rather than when the album is queued.
	//
	// Called on its own goroutine, never on the pump loop.
	Resolve func(ctx context.Context, t store.Track) (string, error)

	// resolveGen numbers lookups so a slow one cannot answer for a track the
	// listener has already moved off. It replaces a single in-flight flag,
	// which used to make every play pressed during a lookup do nothing at all.
	resolveGen     atomic.Int64
	pendingResolve int // queue entry to open once the current audio drains, or -1
	freshAt        int // index whose expiring location was just resolved, or -1
}

type switchPoint struct {
	atFrame int64
	index   int
}

func New(sink Sink) *Player {
	p := &Player{
		cmds:           make(chan command, 8),
		sink:           sink,
		subs:           map[chan Event]struct{}{},
		volume:         1,
		repeat:         "off",
		index:          -1,
		pendingResolve: -1,
		freshAt:        -1,
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

// Clear empties the queue and stops playback.
func (p *Player) Clear() { p.send(command{kind: "clear"}) }

// Remove drops one entry from the queue. Removing what is playing moves on to
// the next track rather than leaving the player pointing at nothing.
func (p *Player) Remove(index int) { p.send(command{kind: "remove", index: index}) }

// Restore loads a saved queue without starting playback: after a restart the
// music waits for the user, it does not ambush them.
func (p *Player) Restore(q []store.Track, index int, positionMS int64, volume float64, shuffle bool, repeat string) {
	p.send(command{kind: "restore", queue: q, index: index, ms: positionMS, f: volume, b: shuffle, s: repeat})
}

// State asks run() for a snapshot, so readers never touch engine-owned fields.
// State is the settled state after every command already sent. Callers rely on
// that: an endpoint that answers with the player's state has to describe the
// command it just performed, not the one before it.
func (p *Player) State() State {
	reply := make(chan State, 1)
	p.cmds <- command{kind: "state", reply: reply}
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
	// A read changes nothing and must not emit an event of its own.
	if c.kind == "state" {
		c.reply <- p.snapshot()
		return
	}
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
	case "clear":
		p.queue = nil
		p.openAt(-1, 0)
	case "remove":
		p.removeAt(c.index)
	case "pause":
		// Silence has to be immediate, so the buffered tail goes too. Where it
		// stopped is remembered, because resume re-opens the track there.
		if p.playing {
			p.pausedAt = p.positionMS()
			p.playing = false
			p.sink.Discard()
		}
	case "resume":
		// Re-opened at the remembered position: the half second that was thrown
		// away on pause would otherwise be skipped.
		if p.index >= 0 && p.index < len(p.queue) {
			p.openAt(p.index, p.pausedAt)
		}
	case "next":
		p.skipTo(p.index + 1)
	case "prev":
		// Past three seconds, or already on the first track, previous restarts
		// what is playing. It must never step off the front of the queue and
		// leave the player pointing at nothing.
		if p.positionMS() > 3000 || p.index <= 0 {
			p.openAt(max(p.index, 0), 0)
		} else {
			p.openAt(p.index-1, 0)
		}
	case "resolved":
		// The listener may have moved on while the lookup ran; only the entry
		// that was asked for is filled in, and it only starts if it is still
		// the one selected. A newer lookup has already been discarded above.
		if c.index >= 0 && c.index < len(p.queue) {
			p.queue[c.index].Path = c.s
			p.freshAt = c.index // this one location is trusted, once
			if p.index == c.index {
				p.openAt(c.index, 0)
			}
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

// skipTo moves forward through the queue: wrapping when repeat is on, and
// stopping on the last track otherwise — never emptying a queue that still has
// tracks in it.
func (p *Player) skipTo(index int) {
	if len(p.queue) == 0 {
		p.openAt(-1, 0)
		return
	}
	switch {
	case index < len(p.queue):
		p.openAt(index, 0)
	case p.repeat == "all":
		p.openAt(0, 0)
	default:
		p.openAt(len(p.queue)-1, 0)
		p.playing = false // parked at the start of the last track, ready to resume
	}
}

// openAt starts index at offsetMS, discarding whatever is buffered.
func (p *Player) openAt(index int, offsetMS int64) {
	if p.dec != nil {
		p.dec.Close()
		p.dec = nil
	}
	// Drop what the device still holds of the old track. Without this a skip is
	// inaudible for half a second and the position counter runs against audio
	// nobody is hearing any more.
	p.sink.Discard()
	p.switches = nil
	p.pending = nil
	p.drainTo = 0
	p.playing = false
	if index < 0 || index >= len(p.queue) {
		p.index = -1
		return
	}
	// No file, or a link that may since have died: ask for a fresh one. The
	// player never replays an expiring URL it was given earlier.
	if t := p.queue[index]; t.Path == "" || (t.Ephemeral && index != p.freshAt) {
		p.startResolve(index)
		return
	}
	// Freshness is spent on use: leaving this track and coming back has to ask
	// the source again, because by then the link may be dead.
	if p.queue[index].Ephemeral {
		p.freshAt = -1
	}
	d, err := audio.Open(p.queue[index].Path)
	if err != nil && p.queue[index].Ephemeral && index == p.freshAt {
		// The link was resolved moments ago and still failed. One more attempt
		// with a brand new one, then it is reported rather than retried again.
		log.Printf("player: %s went stale, resolving again: %v", p.queue[index].Title, err)
		p.queue[index].Path = ""
		p.freshAt = -1
		p.startResolve(index)
		return
	}
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

// startResolve looks a track up off the engine goroutine and re-opens it when
// the location comes back. The index is parked and marked not-playing so the
// interface can say it is finding the stream rather than appearing stuck.
func (p *Player) startResolve(index int) {
	p.index = index
	p.playing = false
	if p.Resolve == nil {
		p.emit(Event{Type: "error", Message: "no source can play this track"})
		return
	}
	gen := p.resolveGen.Add(1)
	t := p.queue[index]
	p.emit(Event{Type: "resolving", Message: t.Title})
	go func() {
		loc, err := p.Resolve(context.Background(), t)
		if p.resolveGen.Load() != gen {
			return // the listener picked something else while this ran
		}
		if err != nil || loc == "" {
			p.emit(Event{Type: "error", Message: "no stream found for " + t.Title})
			return
		}
		p.send(command{kind: "resolved", index: index, s: loc})
	}()
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
	if p.dec != nil {
		p.dec.Close()
		p.dec = nil
	}
	if len(p.queue) == 0 {
		p.playing = false
		p.emitState()
		return
	}

	// Loop rather than recurse: a run of unreadable files used to re-enter this
	// function with a nil decoder, which dereferenced nil and took the process
	// with it. Every candidate gets one try, then playback stops.
	for tries := 0; tries < len(p.queue); tries++ {
		next := p.index + 1
		if p.repeat == "one" {
			next = p.index
		} else if next >= len(p.queue) {
			if p.repeat != "all" {
				// Keep playing until the ring has actually drained; the last
				// ~500ms of audio is still queued in the sink.
				p.drainTo = p.pushed
				return
			}
			next = 0
		}

		if p.queue[next].Path == "" {
			// Nothing to hand the decoder yet. Let the buffered audio finish,
			// then pick the track up once it has been looked up — a lookup
			// takes seconds, so there is no gapless join to preserve.
			p.pendingResolve = next
			p.drainTo = p.pushed
			return
		}
		d, err := audio.Open(p.queue[next].Path)
		if err != nil {
			p.emit(Event{Type: "error", Message: err.Error()})
			p.index = next // skip the bad file and try the one after it
			continue
		}
		p.dec = d
		// The old track is still in the ring; the position flips to the new track
		// only once the sink has actually consumed up to this frame.
		p.switches = append(p.switches, switchPoint{atFrame: p.pushed, index: next})
		return
	}

	p.playing = false
	p.emitState()
}

func (p *Player) advanceSwitches() {
	played := p.sink.Played()
	if p.drainTo > 0 && played >= p.drainTo {
		p.drainTo = 0
		p.playing = false
		if next := p.pendingResolve; next >= 0 {
			p.pendingResolve = -1
			p.openAt(next, 0) // an unresolved entry: this starts the lookup
		}
		p.emitState()
	}
	for len(p.switches) > 0 && played >= p.switches[0].atFrame {
		p.index = p.switches[0].index
		p.trackStart = p.switches[0].atFrame
		p.trackOff = 0
		p.switches = p.switches[1:]
		p.emitState()
		p.maybeRefill()
	}
}

// refillAt is how close to the end of the queue the refill is asked for. Two
// tracks is enough time for a query and a network lookup to finish before the
// music would otherwise stop.
const refillAt = 2

// maybeRefill tops up the queue as it runs down. Appends only: whatever is
// already queued keeps its place, so this never jumps ahead of a queue the
// listener built themselves.
func (p *Player) maybeRefill() {
	if p.Suggest == nil || p.repeat != "off" || p.index < 0 {
		return
	}
	if len(p.queue)-p.index > refillAt {
		return
	}
	// One at a time. Without this every state change near the end of the queue
	// would start another lookup.
	if !p.refilling.CompareAndSwap(false, true) {
		return
	}

	seed := p.queue[p.index]
	go func() {
		defer p.refilling.Store(false)
		more := p.Suggest(context.Background(), seed)
		if len(more) > 0 {
			// Back in through the command channel, so the queue is only ever
			// touched by the goroutine that owns it.
			p.Enqueue(more)
		}
	}()
}

// removeAt drops a queue entry, keeping the current track playing unless it is
// the one being removed.
func (p *Player) removeAt(index int) {
	if index < 0 || index >= len(p.queue) {
		return
	}
	current := p.index
	p.queue = append(p.queue[:index:index], p.queue[index+1:]...)
	switch {
	case index > current:
		// Nothing playing changes.
	case index < current:
		p.index--
		// The decoder keeps running; only the index shifts.
	default:
		p.openAt(index, 0) // what was playing is gone; the next track takes its place
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
