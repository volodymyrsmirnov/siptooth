package echo

import (
	"math"
	"testing"
)

const blk = BlockSamples

// toneBlocks returns n 20 ms blocks of a sine at freq and amplitude (0..1 of full scale).
func toneBlocks(freq float64, n int, amp float64) [][]int16 {
	s := make([]int16, n*blk)
	for i := range s {
		s[i] = int16(amp * 32767 * math.Sin(2*math.Pi*freq*float64(i)/8000))
	}
	out := make([][]int16, n)
	for i := range out {
		out[i] = s[i*blk : (i+1)*blk]
	}
	return out
}

func scale(b []int16, db float64) []int16 {
	f := math.Pow(10, db/20)
	out := make([]int16, len(b))
	for i, s := range b {
		out[i] = int16(float64(s) * f)
	}
	return out
}

func mix(a, b []int16) []int16 {
	out := make([]int16, blk)
	for i := range out {
		v := 0
		if a != nil {
			v += int(a[i])
		}
		if b != nil {
			v += int(b[i])
		}
		out[i] = int16(max(-32768, min(32767, v)))
	}
	return out
}

func isSilent(b []int16) bool {
	for _, s := range b {
		if s != 0 {
			return false
		}
	}
	return true
}

func same(a, b []int16) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

// scene simulates the bridge: per 20 ms block the played reference is fed (in SCO-sized
// 30-sample pieces, as the bridge writes it) and the incoming block is gated. The incoming
// block is the reference delayed by delay blocks and attenuated by echoDB, plus caller[i].
func scene(g *Gate, ref, caller [][]int16, delay int, echoDB float64) (in, out [][]int16) {
	n := max(len(ref), len(caller))
	for i := 0; i < n; i++ {
		r := make([]int16, blk)
		if i < len(ref) && ref[i] != nil {
			r = ref[i]
		}
		for k := 0; k < blk; k += 30 {
			g.Reference(r[k:min(k+30, blk)])
		}
		var echo, c []int16
		if j := i - delay; j >= 0 && j < len(ref) && ref[j] != nil {
			echo = scale(ref[j], echoDB)
		}
		if i < len(caller) {
			c = caller[i]
		}
		b := mix(echo, c)
		in = append(in, b)
		out = append(out, g.Process(b))
	}
	return in, out
}

// A pure echo, 10 dB below the played audio and 200 ms late, is fully gated, including the
// tail after playback stops.
func TestEchoGatePureEchoGated(t *testing.T) {
	g := NewGate(0, 600)
	ref := toneBlocks(440, 100, 0.3) // 2 s of assistant speech
	in, out := scene(g, append(ref, make([][]int16, 50)...), nil, 10, -10)
	for i := range out {
		if !isSilent(in[i]) && !isSilent(out[i]) {
			t.Fatalf("echo block %d passed", i)
		}
	}
	st := g.Stats()
	if st.Passed != 0 || st.Gated < 100 {
		t.Fatalf("stats %+v", st)
	}
	if st.DiffMedianDB < -11 || st.DiffMedianDB > -9 || st.DiffMaxDB > -9 {
		t.Fatalf("diffs %+v", st)
	}
}

// Caller speech louder than the reference while the assistant plays passes after the
// two-block onset, and a 100 ms pause inside it is not chopped.
func TestEchoGateDoubleTalkPasses(t *testing.T) {
	g := NewGate(0, 600)
	ref := toneBlocks(440, 150, 0.1)
	speech := toneBlocks(800, 100, 0.5)
	caller := make([][]int16, 150)
	for i := 50; i < 100; i++ {
		if i < 75 || i >= 80 { // 100 ms pause at 75..79
			caller[i] = speech[i-50]
		}
	}
	in, out := scene(g, ref, caller, 10, -10)
	for i := 10; i < 50; i++ {
		if !isSilent(out[i]) {
			t.Fatalf("echo block %d passed", i)
		}
	}
	if !isSilent(out[50]) {
		t.Fatal("the first block above the threshold is the onset")
	}
	for i := 51; i < 100; i++ {
		if !same(in[i], out[i]) {
			t.Fatalf("caller block %d chopped", i)
		}
	}
	// 300 ms after the speech the gate is closed again: the echo is gated.
	for i := 100 + 15; i < 150; i++ {
		if !isSilent(out[i]) {
			t.Fatalf("echo block %d after the speech passed", i)
		}
	}
	if st := g.Stats(); st.Passed < 49 {
		t.Fatalf("stats %+v", st)
	}
}

// Without assistant audio the gate never touches the caller, however quiet.
func TestEchoGateNoReferenceAlwaysPasses(t *testing.T) {
	g := NewGate(0, 600)
	caller := toneBlocks(300, 50, 0.01)
	in, out := scene(g, nil, caller, 10, -10)
	for i := range out {
		if !same(in[i], out[i]) {
			t.Fatalf("block %d changed", i)
		}
	}
	if st := g.Stats(); st.Gated != 0 || st.Passed != 0 {
		t.Fatalf("stats only count while the reference is active: %+v", st)
	}
}

// After playback stops the reference stays active for the window (the echo tail is gated);
// once the window has passed, caller speech quieter than the old reference passes.
func TestEchoGateTail(t *testing.T) {
	g := NewGate(0, 600)
	ref := toneBlocks(440, 50, 0.3)
	quiet := toneBlocks(800, 100, 0.05)
	caller := make([][]int16, 150)
	for i := 60; i < 150; i++ {
		caller[i] = quiet[i-60]
	}
	in, out := scene(g, ref, caller, 10, -10)
	for i := 50; i < 60; i++ { // echo tail
		if !isSilent(out[i]) {
			t.Fatalf("tail block %d passed", i)
		}
	}
	// the last reference block (49) leaves the 30-block window at block 79
	for i := 80; i < 150; i++ {
		if !same(in[i], out[i]) {
			t.Fatalf("caller block %d gated after the window", i)
		}
	}
}

// The threshold moves the decision: at -20 dB a -10 dB "echo" counts as the caller.
func TestEchoGateThreshold(t *testing.T) {
	g := NewGate(-20, 600)
	ref := toneBlocks(440, 60, 0.3)
	in, out := scene(g, ref, nil, 10, -10)
	for i := 12; i < 60; i++ {
		if !same(in[i], out[i]) {
			t.Fatalf("block %d gated", i)
		}
	}
}

func TestEchoGateStatsEmpty(t *testing.T) {
	if st := NewGate(0, 600).Stats(); st != (Stats{}) {
		t.Fatalf("%+v", st)
	}
}

// Silent incoming blocks (below the noise floor) are gated and counted, but stay out of the
// level differences: they say nothing about how loud the echo is.
func TestEchoGateStatsSkipSilentBlocks(t *testing.T) {
	g := NewGate(0, 600)
	ref := toneBlocks(440, 30, 0.3)
	scene(g, ref, nil, 10, -10) // blocks 0..9 are silent: the echo has not arrived yet
	st := g.Stats()
	if st.Gated != 30 {
		t.Fatalf("stats %+v", st)
	}
	if st.DiffMedianDB < -11 || st.DiffP90DB < -11 || st.DiffMaxDB > -9 || st.DiffMaxDB < -11 {
		t.Fatalf("silent blocks in the diffs: %+v", st)
	}
	g = NewGate(0, 600)
	scene(g, ref, nil, 40, -10) // only silence while the reference is active
	if st := g.Stats(); st.Gated != 30 || st.DiffMedianDB != 0 || st.DiffMaxDB != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// The echo delay is estimated from the level envelopes: a reference with
// distinct bursts returned 300 ms late is found at 300 ms.
func TestGateEchoDelay(t *testing.T) {
	g := NewGate(6, 600)
	var ref [][]int16
	loud := toneBlocks(440, 400, 0.3)
	for i := 0; i < 400; i++ {
		// irregular speech-like bursts so only the true lag lines up
		if (i*7+i*i/13)%5 < 2 {
			ref = append(ref, loud[i])
		} else {
			ref = append(ref, nil)
		}
	}
	scene(g, ref, nil, 15, -10)
	st := g.Stats()
	if st.DelayMs != 300 {
		t.Fatalf("delay %d ms, want 300 (corr %.2f)", st.DelayMs, st.DelayCorr)
	}
	if st.DelayCorr < 0.8 {
		t.Fatalf("corr %.2f", st.DelayCorr)
	}
}

// Without echo there is nothing to line up: the correlation stays low.
func TestGateEchoDelayNoEcho(t *testing.T) {
	g := NewGate(6, 600)
	ref := toneBlocks(440, 300, 0.3)
	for i := range ref {
		if i%10 > 4 {
			ref[i] = nil
		}
	}
	scene(g, ref, nil, 0, -200)
	if st := g.Stats(); st.DelayCorr > 0.3 {
		t.Fatalf("corr %.2f without echo", st.DelayCorr)
	}
}
