// Package echo gates the line echo of the far end's own voice out of the
// caller audio (spec 6.6), copied from allochka's audio.EchoGate.
package echo

import (
	"math"
	"sort"
	"sync"
)

// BlockSamples is the gate's block: 20 ms of 8 kHz PCM.
const BlockSamples = 8000 / 50

const (
	// echoNoiseFloor is the RMS below which an incoming block never counts as the caller, and
	// below which the reference counts as silence (the playback buffer pads with silence).
	echoNoiseFloor = 50
	// echoOnsetBlocks consecutive blocks above the threshold open the gate.
	echoOnsetBlocks = 2
	// echoHangBlocks blocks (300 ms) below the threshold close an open gate again.
	echoHangBlocks = 15
	// echoMaxDiffs bounds the stored per-block differences (about 30 minutes of gated blocks).
	echoMaxDiffs = 90000
	// envMax bounds the level envelopes kept for the delay estimate (3 minutes).
	envMax = 9000
	// maxLagBlocks is the longest echo delay looked for (1 s).
	maxLagBlocks = 50
)

// Gate suppresses the echo of the far end's own voice that the phone call returns on the
// line (allochka spec section 4, 2026-10-05 iteration 4; siptooth spec 6.6). It keeps the RMS of every 20 ms block of the
// audio actually played to the phone (the reference) and treats the loudest reference block of
// the last window as the expected echo level: an incoming block quieter than that level times
// the threshold is echo and is replaced with silence. Real double-talk is not chopped: two
// consecutive blocks above the threshold open the gate, and it stays open until 300 ms of
// blocks below it. Without assistant audio in the window the gate is always open.
//
// Reference and Process are meant to be called from one goroutine (the media pump); Stats
// may be called from any goroutine.
type Gate struct {
	mu        sync.Mutex
	factor    float64   // 10^(threshold_db/20)
	ref       []float64 // ring of reference block RMS values, one per 20 ms block
	refPos    int
	partial   []int16 // reference samples not yet forming a full block
	open      bool
	aboveRun  int
	belowRun  int
	refActive bool // the window held assistant audio at the previous block

	gated, passed int
	diffs         []float64 // incoming dB - reference dB of gated blocks

	refEnv, inEnv []float64 // per-block RMS of the reference and the raw input, for the delay estimate
}

// NewGate returns a gate with the given threshold (dB relative to the reference) and
// reference window (milliseconds, rounded up to whole 20 ms blocks).
func NewGate(thresholdDB float64, windowMs int) *Gate {
	n := (windowMs + 19) / 20
	if n < 1 {
		n = 1
	}
	return &Gate{factor: math.Pow(10, thresholdDB/20), ref: make([]float64, n), open: true,
		partial: make([]int16, 0, BlockSamples)}
}

// Reference feeds PCM that was just written to the phone (any length).
func (g *Gate) Reference(pcm []int16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for len(pcm) > 0 {
		k := min(BlockSamples-len(g.partial), len(pcm))
		g.partial = append(g.partial, pcm[:k]...)
		pcm = pcm[k:]
		if len(g.partial) == BlockSamples {
			g.ref[g.refPos] = rms(g.partial)
			if len(g.refEnv) < envMax {
				g.refEnv = append(g.refEnv, g.ref[g.refPos])
			}
			g.refPos = (g.refPos + 1) % len(g.ref)
			g.partial = g.partial[:0]
		}
	}
}

// Process gates one block of incoming caller PCM (normally BlockSamples long). It returns
// in unchanged when it passes, or a block of silence of the same length when it is echo.
func (g *Gate) Process(in []int16) []int16 {
	g.mu.Lock()
	defer g.mu.Unlock()
	level := rms(in)
	if len(g.inEnv) < envMax {
		g.inEnv = append(g.inEnv, level)
	}
	refMax := 0.0
	for _, r := range g.ref {
		refMax = max(refMax, r)
	}
	if refMax < echoNoiseFloor { // no assistant audio within the window: always open
		g.open, g.aboveRun, g.belowRun, g.refActive = true, 0, 0, false
		return in
	}
	if !g.refActive { // assistant audio just started: expect echo
		g.open, g.aboveRun, g.belowRun, g.refActive = false, 0, 0, true
	}
	above := level >= echoNoiseFloor && level > refMax*g.factor
	pass := false
	if g.open {
		if above {
			g.belowRun = 0
			pass = true
		} else {
			g.belowRun++
			if g.belowRun >= echoHangBlocks {
				g.open, g.aboveRun = false, 0
			} else {
				pass = true // hangover: pauses inside the caller's speech are kept
			}
		}
	} else {
		if above {
			g.aboveRun++
			if g.aboveRun >= echoOnsetBlocks {
				g.open, g.belowRun = true, 0
				pass = true
			}
		} else {
			g.aboveRun = 0
		}
	}
	if pass {
		g.passed++
		return in
	}
	g.gated++
	if level >= echoNoiseFloor && len(g.diffs) < echoMaxDiffs { // silence says nothing about the echo level
		g.diffs = append(g.diffs, 20*math.Log10(level/refMax))
	}
	return make([]int16, len(in))
}

// Stats describes the gate's work while far-end audio was in the reference window.
type Stats struct {
	Gated  int // blocks replaced with silence
	Passed int // blocks passed to the far end
	// Incoming level minus reference level (dB) over the gated blocks that carried sound
	// (RMS at least the noise floor); zero without any.
	DiffMedianDB float64
	DiffP90DB    float64
	DiffMaxDB    float64
	// The lag (0 to 1 s, 20 ms steps) at which the input level envelope best follows the
	// reference envelope over the first 3 minutes, and that correlation (-1..1). A short
	// delay points at the phone or the Bluetooth link, a long one at the network or the
	// caller's device; a low correlation means little echo.
	DelayMs   int
	DelayCorr float64
}

// Stats returns the counters so far.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	d := append([]float64(nil), g.diffs...)
	st := Stats{Gated: g.gated, Passed: g.passed}
	st.DelayMs, st.DelayCorr = estimateDelay(g.refEnv, g.inEnv)
	g.mu.Unlock()
	if len(d) == 0 {
		return st
	}
	sort.Float64s(d)
	st.DiffMedianDB = round1(d[len(d)/2])
	st.DiffP90DB = round1(d[min(len(d)-1, len(d)*9/10)])
	st.DiffMaxDB = round1(d[len(d)-1])
	return st
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func rms(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		v := float64(s)
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// estimateDelay returns the lag (ms) at which in best correlates with ref, and the
// correlation; 0, 0 without enough data.
func estimateDelay(ref, in []float64) (int, float64) {
	bestLag, best := 0, 0.0
	for lag := 0; lag <= maxLagBlocks; lag++ {
		n := min(len(ref), len(in)-lag)
		if n < 2*maxLagBlocks {
			break
		}
		if c := pearson(ref[:n], in[lag:lag+n]); c > best {
			bestLag, best = lag, c
		}
	}
	return bestLag * 20, round2(best)
}

func pearson(a, b []float64) float64 {
	n := float64(len(a))
	var sa, sb float64
	for i := range a {
		sa += a[i]
		sb += b[i]
	}
	ma, mb := sa/n, sb/n
	var cov, va, vb float64
	for i := range a {
		da, db := a[i]-ma, b[i]-mb
		cov += da * db
		va += da * da
		vb += db * db
	}
	if va == 0 || vb == 0 {
		return 0
	}
	return cov / math.Sqrt(va*vb)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
