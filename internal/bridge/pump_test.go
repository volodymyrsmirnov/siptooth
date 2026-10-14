package bridge

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
)

type fakeRTP struct {
	mu       sync.Mutex
	frames   [][]int16
	silences int
	pl       *rtp.Playout
}

func newFakeRTP(jitterMs int) *fakeRTP {
	pl := rtp.NewPlayout(rtp.Config{JitterMs: jitterMs})
	pl.SetStartup(true) // as Session.Start leaves it
	return &fakeRTP{pl: pl}
}

func (f *fakeRTP) WriteFrame(pcm []int16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, append([]int16(nil), pcm...))
	return nil
}
func (f *fakeRTP) WriteSilence() error   { f.mu.Lock(); f.silences++; f.mu.Unlock(); return nil }
func (f *fakeRTP) Playout() *rtp.Playout { return f.pl }
func (f *fakeRTP) Stats() rtp.Stats      { return rtp.Stats{} }
func (f *fakeRTP) frameCount() int       { f.mu.Lock(); defer f.mu.Unlock(); return len(f.frames) }
func (f *fakeRTP) silenceCount() int     { f.mu.Lock(); defer f.mu.Unlock(); return f.silences }
func (f *fakeRTP) frame(i int) []int16   { f.mu.Lock(); defer f.mu.Unlock(); return f.frames[i] }

// pushRamp buffers n samples valued 1..n.
func pushRamp(pl *rtp.Playout, n int) {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(i + 1)
	}
	for off := 0; off < n; off += 160 {
		pl.Push(uint32(off), 1, pcm[off:min(off+160, n)])
	}
}

func toBytes(pcm []int16) []byte {
	b := make([]byte, 2*len(pcm))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

func toPCM(b []byte) []int16 {
	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return pcm
}

type pumpRun struct {
	end    chan PumpEnd
	cancel context.CancelFunc
	phone  sco.Link
	clk    *fakeClock
	rtp    *fakeRTP
}

func startPump(t *testing.T, settle time.Duration, r *fakeRTP) *pumpRun {
	t.Helper()
	pumpSide, phone := sco.NewPipe(60)
	ctx, cancel := context.WithCancel(context.Background())
	run := &pumpRun{end: make(chan PumpEnd, 1), cancel: cancel, phone: phone, clk: newFakeClock(), rtp: r}
	go func() { run.end <- RunPump(ctx, pumpSide, r, settle, run.clk) }()
	t.Cleanup(func() { cancel(); phone.Close(); pumpSide.Close() })
	want := 2 // stall and RTP silence tickers
	if settle > 0 {
		want++ // settle timer
	}
	waitFor(t, "pump timers", func() bool { return run.clk.armed() >= want })
	return run
}

// exchange sends one SCO frame and returns the pump's answer.
func (p *pumpRun) exchange(t *testing.T, frame []byte) []byte {
	t.Helper()
	if _, err := p.phone.Write(frame); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	got := make(chan int, 1)
	go func() { n, _ := p.phone.Read(buf); got <- n }()
	select {
	case n := <-got:
		return buf[:n]
	case <-time.After(2 * time.Second):
		t.Fatal("no downlink answer")
		return nil
	}
}

// drain collects everything the pump writes to the phone until stopped.
func (p *pumpRun) drain() (stop func() []byte) {
	var mu sync.Mutex
	var all []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			n, err := p.phone.Read(buf)
			if err != nil {
				return
			}
			mu.Lock()
			all = append(all, buf[:n]...)
			mu.Unlock()
		}
	}()
	return func() []byte {
		p.phone.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return all
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUplinkFrames(t *testing.T) {
	r := newFakeRTP(0)
	p := startPump(t, 0, r)
	var want []int16
	for i := 0; i < 16; i++ {
		pcm := make([]int16, 30)
		for k := range pcm {
			pcm[k] = int16(i*30 + k + 1)
		}
		want = append(want, pcm...)
		p.exchange(t, toBytes(pcm))
	}
	if r.frameCount() != 3 {
		t.Fatalf("frames = %d, want 3", r.frameCount())
	}
	for i := 0; i < 3; i++ {
		f := r.frame(i)
		if len(f) != 160 {
			t.Fatalf("frame %d has %d samples", i, len(f))
		}
		for k, s := range f {
			if s != want[i*160+k] {
				t.Fatalf("frame %d sample %d = %d, want %d", i, k, s, want[i*160+k])
			}
		}
	}
}

func TestDownlinkSameLength(t *testing.T) {
	r := newFakeRTP(0)
	pushRamp(r.pl, 2000)
	p := startPump(t, 0, r)
	for _, n := range []int{60, 58, 30, 60} {
		out := p.exchange(t, make([]byte, n))
		if len(out) != n {
			t.Fatalf("wrote %d bytes for a %d byte read", len(out), n)
		}
	}
}

func TestSettleSilenceThenAudio(t *testing.T) {
	r := newFakeRTP(60)
	pushRamp(r.pl, 8000)
	p := startPump(t, 800*time.Millisecond, r)
	step := 3750 * time.Microsecond
	for i := 0; i < 200; i++ { // 750 ms
		out := p.exchange(t, make([]byte, 60))
		for _, s := range toPCM(out) {
			if s != 0 {
				t.Fatalf("non-silent downlink during settle (read %d)", i)
			}
		}
		p.clk.Advance(step)
	}
	p.clk.Advance(100 * time.Millisecond) // settle elapsed
	time.Sleep(20 * time.Millisecond)     // let the pump see the timer
	out := toPCM(p.exchange(t, make([]byte, 60)))
	for k, s := range out {
		if s != int16(k+1) {
			t.Fatalf("after settle sample %d = %d, want %d", k, s, k+1)
		}
	}
}

func TestStallPlaysBufferedAudio(t *testing.T) {
	r := newFakeRTP(0)
	pushRamp(r.pl, 4000)
	p := startPump(t, 0, r)
	stop := p.drain()
	for i := 0; i < 5; i++ {
		p.clk.Advance(100 * time.Millisecond)
	}
	// let the pump finish the last tick before closing
	time.Sleep(20 * time.Millisecond)
	data := stop()
	pcm := toPCM(data)
	if len(pcm) < 1000 {
		t.Fatalf("only %d samples written during stall", len(pcm))
	}
	for i := 0; i < min(len(pcm), 4000); i++ {
		if pcm[i] != int16(i+1) {
			t.Fatalf("sample %d = %d, want %d", i, pcm[i], i+1)
		}
	}
	// 100 ms to 500 ms of stall at one packet per 20 ms is about 20
	if n := r.silenceCount(); n < 15 || n > 21 {
		t.Fatalf("rtp silences = %d, want about 20", n)
	}
}

func TestStallTimeoutReturns(t *testing.T) {
	r := newFakeRTP(0)
	p := startPump(t, 0, r)
	stop := p.drain()
	defer stop()
	var end PumpEnd
loop:
	for i := 0; i < 60; i++ {
		p.clk.Advance(100 * time.Millisecond)
		select {
		case end = <-p.end:
			break loop
		default:
		}
	}
	if end == "" {
		select {
		case end = <-p.end:
		case <-time.After(time.Second):
			t.Fatal("pump did not return")
		}
	}
	if end != PumpSCOStall {
		t.Fatalf("end = %q, want %q", end, PumpSCOStall)
	}
	if got := p.clk.Now().Sub(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)); got < 5*time.Second || got > 5200*time.Millisecond {
		t.Fatalf("returned after %v, want about 5s", got)
	}
}

func TestEOFReturns(t *testing.T) {
	r := newFakeRTP(0)
	p := startPump(t, 0, r)
	p.phone.Close()
	select {
	case end := <-p.end:
		if end != PumpSCOEOF {
			t.Fatalf("end = %q", end)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return")
	}
}

func TestCtxReturns(t *testing.T) {
	r := newFakeRTP(0)
	p := startPump(t, 0, r)
	p.cancel()
	select {
	case end := <-p.end:
		if end != PumpCtx {
			t.Fatalf("end = %q", end)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return")
	}
}
