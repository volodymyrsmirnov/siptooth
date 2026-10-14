package rtp

import (
	"math"
	"testing"
	"time"
)

func ramp(start, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(start + i + 1)
	}
	return out
}

func tone(n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	return out
}

func readN(p *Playout, n int) []int16 {
	out := make([]int16, n)
	p.Read(out)
	return out
}

func equal(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlayoutReorder(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(1000, 1, ramp(0, 160))
	p.Push(1000+320, 1, ramp(320, 160))
	p.Push(1000+160, 1, ramp(160, 160))
	if got := readN(p, 480); !equal(got, ramp(0, 480)) {
		t.Fatal("reordered packets not in timestamp order")
	}
}

func TestPlayoutVariablePacketSizes(t *testing.T) {
	p := NewPlayout(Config{})
	ts, v := uint32(5000), 0
	for _, ms := range []int{10, 20, 30, 60} {
		n := ms * 8
		p.Push(ts, 1, ramp(v, n))
		ts += uint32(n)
		v += n
	}
	if got := readN(p, v); !equal(got, ramp(0, v)) {
		t.Fatal("variable-size packets not contiguous")
	}
}

func TestPlayoutGapSilence(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(0, 1, ramp(0, 160))
	p.Push(960, 1, ramp(160, 160))
	got := readN(p, 1120)
	want := append(append(ramp(0, 160), make([]int16, 800)...), ramp(160, 160)...)
	if !equal(got, want) {
		t.Fatal("expected 800 samples of silence between packets")
	}
}

func TestPlayoutDuplicate(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(0, 1, ramp(0, 160))
	p.Push(0, 1, ramp(0, 160))
	if c := p.counters(); c.duplicate != 1 {
		t.Fatalf("duplicate = %d", c.duplicate)
	}
	if got := readN(p, 320); !equal(got[:160], ramp(0, 160)) || !equal(got[160:], make([]int16, 160)) {
		t.Fatal("duplicate was played twice")
	}
}

func TestPlayoutLate(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(0, 1, ramp(0, 160))
	p.Push(160, 1, ramp(160, 160))
	readN(p, 320)
	p.Push(0, 1, ramp(0, 160)) // entirely before the playout point
	if c := p.counters(); c.late != 1 {
		t.Fatalf("late = %d", c.late)
	}
	if got := readN(p, 160); !equal(got, make([]int16, 160)) {
		t.Fatal("late packet was played")
	}
}

func TestTimestampWrap(t *testing.T) {
	p := NewPlayout(Config{})
	ts := uint32(math.MaxUint32 - 239) // wraps after 240 samples
	for i := 0; i < 4; i++ {
		p.Push(ts+uint32(i*160), 1, ramp(i*160, 160))
	}
	if got := readN(p, 640); !equal(got, ramp(0, 640)) {
		t.Fatal("timestamp wrap broke ordering")
	}
}

func TestStartupBufferKeepsEverything(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 200})
	p.SetStartup(true)
	const total = 13 * 8000
	src := tone(total, 8000)
	for i := 0; i < total; i += 160 {
		p.Push(uint32(i), 1, src[i:i+160])
	}
	if d := p.Depth(); d != 13*time.Second {
		t.Fatalf("depth = %v", d)
	}
	var got []int16
	for len(got) < total {
		got = append(got, readN(p, 30)...)
	}
	if !equal(got[:total], src) {
		t.Fatal("samples lost or reordered in startup mode")
	}
	if c := p.counters(); c.overflow != 0 || c.discarded != 0 {
		t.Fatalf("counters %+v", c)
	}
}

func TestStartupThenSteadyKeepsSpeech(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 200})
	p.SetStartup(true)
	src := tone(13*8000, 8000)
	for i := 0; i < len(src); i += 160 {
		p.Push(uint32(i), 1, src[i:i+160])
	}
	p.SetStartup(false)
	var got []int16
	for len(got) < len(src) {
		got = append(got, readN(p, 30)...)
	}
	if !equal(got[:len(src)], src) {
		t.Fatal("speech skipped after leaving startup")
	}
}

// pattern: alternating 200 ms tone / 200 ms silence, 1 s total.
func altPattern() []int16 {
	var s []int16
	for i := 0; i < 5; i++ {
		if i%2 == 0 {
			s = append(s, tone(1600, 8000)...)
		} else {
			s = append(s, make([]int16, 1600)...)
		}
	}
	return s
}

func TestCatchUpSkipsOnlySilence(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 200})
	src := altPattern()
	for i := 0; i < len(src); i += 160 {
		p.Push(uint32(i), 1, src[i:i+160])
	}
	var got []int16
	for p.Depth() > 0 {
		got = append(got, readN(p, 30)...)
	}
	// Every tone sample must be delivered, in order.
	var wantTone, gotTone []int16
	for _, v := range src {
		if v != 0 {
			wantTone = append(wantTone, v)
		}
	}
	for _, v := range got {
		if v != 0 {
			gotTone = append(gotTone, v)
		}
	}
	if !equal(wantTone, gotTone) {
		t.Fatal("tone samples were dropped or reordered")
	}
	c := p.counters()
	if c.discarded == 0 || c.discarded%160 != 0 {
		t.Fatalf("discarded = %d, want whole 20 ms blocks > 0", c.discarded)
	}
	if int(c.discarded) > 3200 {
		t.Fatalf("skipped more than the silence: %d", c.discarded)
	}
}

func TestCatchUpStopsAtTarget(t *testing.T) {
	// 400 ms of pure silence: catch-up skips blocks until depth <= 60 ms.
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 200})
	for i := 0; i < 400*8; i += 160 {
		p.Push(uint32(i), 1, make([]int16, 160))
	}
	readN(p, 1)
	if d := p.Depth(); d > 60*time.Millisecond {
		t.Fatalf("depth after catch-up = %v", d)
	}
	if d := p.Depth(); d < 40*time.Millisecond {
		t.Fatalf("skipped too much: %v", d)
	}
}

func TestCatchUpDisabled(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 0})
	for i := 0; i < 400*8; i += 160 {
		p.Push(uint32(i), 1, make([]int16, 160))
	}
	readN(p, 1)
	if c := p.counters(); c.discarded != 0 {
		t.Fatalf("discarded %d with catch-up disabled", c.discarded)
	}
}

func TestHardCapOverflow(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60, SilenceRMS: 0})
	src := ramp(0, 160)
	for i := 0; i < 25*8000; i += 160 {
		p.Push(uint32(i), 1, src)
	}
	if d := p.Depth(); d != 20*time.Second {
		t.Fatalf("depth = %v, want 20s", d)
	}
	if c := p.counters(); c.overflow != 5*8000 {
		t.Fatalf("overflow = %d", c.overflow)
	}
}

func TestSSRCChangeContinues(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(100, 1, ramp(0, 160))
	p.Push(9_000_000, 2, ramp(160, 160)) // unrelated timestamp base
	if got := readN(p, 320); !equal(got, ramp(0, 320)) {
		t.Fatal("new SSRC did not continue after the buffered audio")
	}
}

func TestPlayoutUnderrunThenResume(t *testing.T) {
	p := NewPlayout(Config{})
	if got := readN(p, 30); !equal(got, make([]int16, 30)) {
		t.Fatal("empty buffer must read silence")
	}
	p.Push(0, 1, ramp(0, 160))
	readN(p, 160)
	readN(p, 50)                   // underrun
	p.Push(210, 1, ramp(210, 160)) // the clock ran 50 samples through the underrun
	if got := readN(p, 160); !equal(got, ramp(210, 160)) {
		t.Fatal("audio after underrun was lost")
	}
}

func TestStartupReorderBeforeOrigin(t *testing.T) {
	p := NewPlayout(Config{})
	p.SetStartup(true)
	p.Push(320, 1, ramp(320, 160))
	p.Push(160, 1, ramp(160, 160))
	p.Push(0, 1, ramp(0, 160))
	if got := readN(p, 480); !equal(got, ramp(0, 480)) {
		t.Fatal("earlier packets arriving after the first were lost")
	}
	if c := p.counters(); c.late != 0 {
		t.Fatalf("late = %d", c.late)
	}
}

func TestUnderrunAdvancesClock(t *testing.T) {
	p := NewPlayout(Config{SilenceRMS: 0})
	p.Push(0, 1, ramp(0, 160))
	readN(p, 160)
	readN(p, 800)                  // 100 ms of nothing: the clock keeps running
	p.Push(800, 1, ramp(800, 160)) // missed its slot entirely
	if c := p.counters(); c.late != 1 {
		t.Fatalf("late = %d, want 1", c.late)
	}
	p.Push(960, 1, ramp(960, 160))
	if got := readN(p, 160); !equal(got, ramp(960, 160)) {
		t.Fatal("elapsed silence was replayed after the underrun")
	}
}

func TestStartupUnderrunKeepsEverything(t *testing.T) {
	p := NewPlayout(Config{})
	p.SetStartup(true)
	p.Push(0, 1, ramp(0, 160))
	readN(p, 160)
	readN(p, 800)
	p.Push(160, 1, ramp(160, 160))
	if got := readN(p, 160); !equal(got, ramp(160, 160)) {
		t.Fatal("startup mode dropped audio after an underrun")
	}
}

func TestPrimingToJitterTarget(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60}) // 480 samples
	zero := make([]int16, 160)
	p.Push(0, 1, ramp(0, 160))
	if got := readN(p, 160); !equal(got, zero) {
		t.Fatal("played at 20 ms depth")
	}
	p.Push(160, 1, ramp(160, 160))
	if got := readN(p, 160); !equal(got, zero) {
		t.Fatal("played at 40 ms depth")
	}
	p.Push(320, 1, ramp(320, 160))
	if got := readN(p, 480); !equal(got, ramp(0, 480)) {
		t.Fatal("did not play once the target was reached")
	}
	readN(p, 160) // drain: underrun, clock advances to 640
	p.Push(640, 1, ramp(640, 160))
	if got := readN(p, 160); !equal(got, zero) {
		t.Fatal("did not re-prime after a drain")
	}
	p.Push(800, 1, ramp(800, 160))
	p.Push(960, 1, ramp(960, 160))
	if got := readN(p, 160); !equal(got, ramp(640, 160)) {
		t.Fatal("did not resume after re-priming")
	}
}

func TestPrimingAfterStartup(t *testing.T) {
	p := NewPlayout(Config{JitterMs: 60})
	p.SetStartup(true)
	p.Push(0, 1, ramp(0, 160))
	p.SetStartup(false)
	if got := readN(p, 160); !equal(got, make([]int16, 160)) {
		t.Fatal("played before reaching the jitter target after startup")
	}
}

func TestSSRCChangeAfterRead(t *testing.T) {
	p := NewPlayout(Config{})
	p.Push(0, 1, ramp(0, 320))
	readN(p, 160) // readPos != writeEnd
	p.Push(5_000_000, 2, ramp(320, 160))
	if got := readN(p, 320); !equal(got, ramp(160, 320)) {
		t.Fatal("new SSRC must continue after the unplayed audio")
	}
}

func TestStartupRebaseBounded(t *testing.T) {
	p := NewPlayout(Config{})
	p.SetStartup(true)
	p.Push(1<<30, 1, ramp(0, 160))
	p.Push(0, 1, ramp(0, 160)) // ~2^30 samples behind: dropped, not rebased
	if c := p.counters(); c.late != 1 {
		t.Fatalf("late = %d", c.late)
	}
	if d := p.Depth(); d != 20*time.Millisecond {
		t.Fatalf("depth = %v", d)
	}
}

func TestFlushPlaysRemnantBelowTarget(t *testing.T) {
	pl := NewPlayout(Config{JitterMs: 60})
	pcm := make([]int16, 320) // 40 ms
	for i := range pcm {
		pcm[i] = 7
	}
	pl.Push(0, 1, pcm)
	out := make([]int16, 160)
	pl.Read(out)
	if out[0] != 0 {
		t.Fatal("below-target audio played without Flush (priming expected)")
	}
	pl.Flush()
	pl.Read(out)
	if out[0] != 7 {
		t.Fatalf("Flush did not release the remnant: %d", out[0])
	}
	pl.SetStartup(true)
	pl.SetStartup(false) // priming stays off for the rest of the playout's life
	pl.Push(320, 1, pcm)
	pl.Read(out)
	if out[0] != 7 {
		t.Fatal("playout re-primed after SetStartup(false)")
	}
}
