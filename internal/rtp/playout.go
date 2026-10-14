package rtp

import (
	"math"
	"sync"
	"time"
)

const (
	sampleRate      = 8000
	blockSamples    = 160 // 20 ms
	catchUpMargin   = 40 * sampleRate / 1000
	defaultCapSecs  = 20
	samplesPerMilli = sampleRate / 1000
)

// Config tunes the playout buffer.
type Config struct {
	JitterMs          int // steady-state target depth
	SilenceRMS        int // catch-up threshold on the 16-bit scale; 0 disables catch-up
	StartupCapSeconds int // buffer cap in startup mode (default 20)
	HardCapSeconds    int // buffer cap in steady state (default 20)
}

type playoutCounters struct {
	late, duplicate, overflow, discarded uint64
}

// Playout is a timestamp-indexed sample ring. Samples live at absolute
// offsets (RTP timestamp relative to the epoch's first packet, 32-bit wrap
// unrolled); Read consumes from the playout point. Offsets that were never
// filled read as silence, so timestamp gaps play as silence.
type Playout struct {
	mu  sync.Mutex
	cfg Config

	ring   []int16
	filled []bool
	size   int64

	startup bool

	haveEpoch bool
	ssrc      uint32
	refTS     uint32
	refOff    int64

	readPos  int64 // next sample to play
	writeEnd int64 // one past the highest buffered sample
	catching bool
	primed   bool // depth reached the jitter target since the last drain
	played   bool // a Read has consumed buffered audio; the origin is fixed from then on

	c playoutCounters
}

// NewPlayout creates a playout buffer in steady-state mode.
func NewPlayout(cfg Config) *Playout {
	if cfg.StartupCapSeconds <= 0 {
		cfg.StartupCapSeconds = defaultCapSecs
	}
	if cfg.HardCapSeconds <= 0 {
		cfg.HardCapSeconds = defaultCapSecs
	}
	secs := max(cfg.StartupCapSeconds, cfg.HardCapSeconds)
	// Slack beyond the cap so one packet never wraps onto unread audio.
	size := int64(secs)*sampleRate + 2*sampleRate
	return &Playout{
		cfg:    cfg,
		ring:   make([]int16, size),
		filled: make([]bool, size),
		size:   size,
	}
}

func (p *Playout) limit() int64 {
	if p.startup {
		return int64(p.cfg.StartupCapSeconds) * sampleRate
	}
	return int64(p.cfg.HardCapSeconds) * sampleRate
}

// SetStartup switches startup mode: everything is kept and catch-up is off.
func (p *Playout) SetStartup(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startup = on
	p.catching = false
	if !on {
		p.primed = false // re-prime to the jitter target after startup
	}
	p.enforceLimit()
}

// Depth is the buffered audio from the playout point to the newest sample.
func (p *Playout) Depth() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return samplesToDuration(p.depth())
}

func samplesToDuration(n int64) time.Duration {
	return time.Duration(n) * time.Second / sampleRate
}

func (p *Playout) depth() int64 {
	if d := p.writeEnd - p.readPos; d > 0 {
		return d
	}
	return 0
}

func (p *Playout) counters() playoutCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.c
}

func (p *Playout) idx(off int64) int64 { return ((off % p.size) + p.size) % p.size }

// clearRange zeroes [from, to) in the ring.
func (p *Playout) clearRange(from, to int64) {
	for o := from; o < to; o++ {
		i := p.idx(o)
		p.ring[i] = 0
		p.filled[i] = false
	}
}

// enforceLimit drops the oldest samples beyond the mode's cap.
func (p *Playout) enforceLimit() {
	if over := p.depth() - p.limit(); over > 0 {
		p.clearRange(p.readPos, p.readPos+over)
		p.readPos += over
		p.c.overflow += uint64(over)
	}
}

// Push places pcm at the position given by its RTP timestamp. An SSRC change
// starts a new epoch that continues after the audio already buffered (spec 6.3:
// buffered speech is never overwritten).
func (p *Playout) Push(ts uint32, ssrc uint32, pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.haveEpoch || ssrc != p.ssrc {
		p.startEpoch(ts, ssrc, max(p.readPos, p.writeEnd))
	}
	off := p.refOff + int64(int32(ts-p.refTS))
	if off-p.readPos >= p.limit() {
		// Absurd forward jump: re-anchor behind the buffered audio rather than
		// discarding it.
		p.startEpoch(ts, ssrc, max(p.readPos, p.writeEnd))
		off = p.refOff
	}
	p.refTS, p.refOff = ts, off

	// Until playback begins, an earlier timestamp arriving after a later one
	// (reordering at the start of the stream) moves the origin down instead
	// of being dropped: spec 6.3, nothing in the startup buffer is lost.
	if !p.played && off < p.readPos {
		p.readPos = off
	}

	end := off + int64(len(pcm))
	if end <= p.readPos {
		p.c.late++
		return
	}
	if off < p.readPos {
		pcm = pcm[p.readPos-off:]
		off = p.readPos
	}
	if p.rangeFilled(off, end) {
		p.c.duplicate++
		return
	}
	for k, s := range pcm {
		i := p.idx(off + int64(k))
		p.ring[i] = s
		p.filled[i] = true
	}
	if end > p.writeEnd {
		p.writeEnd = end
	}
	p.enforceLimit()
}

func (p *Playout) startEpoch(ts, ssrc uint32, at int64) {
	p.haveEpoch = true
	p.ssrc = ssrc
	p.refTS = ts
	p.refOff = at
}

func (p *Playout) rangeFilled(from, to int64) bool {
	for o := from; o < to; o++ {
		if !p.filled[p.idx(o)] {
			return false
		}
	}
	return true
}

// Read fills out with the next samples. Missing audio (underrun or a
// timestamp gap) is silence.
func (p *Playout) Read(out []int16) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.startup {
		if !p.primed {
			if p.played {
				// Elapsed silence in front of the first buffered audio is not owed.
				for p.readPos < p.writeEnd && !p.filled[p.idx(p.readPos)] {
					p.readPos++
				}
			}
			if p.depth() < int64(p.cfg.JitterMs)*samplesPerMilli {
				clear(out) // priming: wait for the jitter target
				return
			}
			p.primed = true
		}
		p.catchUp()
	}
	if !p.played && p.readPos < p.writeEnd {
		p.played = true
	}
	// In steady state the playout clock keeps running through underruns and
	// timestamp gaps, so elapsed silence is never replayed later and packets
	// arriving past their deadline are late. Startup mode keeps everything and
	// stays put at the end of the buffered audio.
	advance := p.played && !p.startup
	for k := range out {
		if p.readPos >= p.writeEnd {
			out[k] = 0
			if !p.startup {
				p.primed = false // drained: prime again before resuming
			}
			if advance {
				p.readPos++
			}
			continue
		}
		i := p.idx(p.readPos)
		out[k] = p.ring[i]
		p.ring[i] = 0
		p.filled[i] = false
		p.readPos++
	}
}

// catchUp skips whole silent 20 ms blocks while the depth exceeds the target
// plus a margin, until it is back at the target. Speech is never skipped.
func (p *Playout) catchUp() {
	if p.startup || p.cfg.SilenceRMS <= 0 {
		p.catching = false
		return
	}
	target := int64(p.cfg.JitterMs) * samplesPerMilli
	if d := p.depth(); d > target+catchUpMargin {
		p.catching = true
	} else if d <= target {
		p.catching = false
	}
	for p.catching && p.depth() > target && p.depth() >= blockSamples {
		if p.blockRMS(p.readPos) >= float64(p.cfg.SilenceRMS) {
			return
		}
		p.clearRange(p.readPos, p.readPos+blockSamples)
		p.readPos += blockSamples
		p.c.discarded += blockSamples
	}
}

func (p *Playout) blockRMS(from int64) float64 {
	var sum float64
	for o := from; o < from+blockSamples; o++ {
		v := float64(p.ring[p.idx(o)])
		sum += v * v
	}
	return math.Sqrt(sum / blockSamples)
}
