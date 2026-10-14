package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/echo"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
)

type fakeRTP struct {
	mu       sync.Mutex
	frames   [][]int16
	silences int
	pl       *rtp.Playout

	clk          *fakeClock              // set before the pump starts
	silenceTimes []time.Time             // clock time of each WriteSilence
	attempts     int                     // WriteFrame calls, including failed ones
	frameErr     func(attempt int) error // nil: never fail; attempt counts from 1
}

func newFakeRTP(jitterMs int) *fakeRTP {
	pl := rtp.NewPlayout(rtp.Config{JitterMs: jitterMs})
	pl.SetStartup(true) // as Session.Start leaves it
	return &fakeRTP{pl: pl}
}

func (f *fakeRTP) WriteFrame(pcm []int16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.frameErr != nil {
		if err := f.frameErr(f.attempts); err != nil {
			return err
		}
	}
	f.frames = append(f.frames, append([]int16(nil), pcm...))
	return nil
}
func (f *fakeRTP) WriteSilence() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.silences++
	if f.clk != nil {
		f.silenceTimes = append(f.silenceTimes, f.clk.Now())
	}
	return nil
}
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
	return startPumpGate(t, settle, r, nil)
}

func startPumpGate(t *testing.T, settle time.Duration, r *fakeRTP, gate *echo.Gate) *pumpRun {
	t.Helper()
	pumpSide, phone := sco.NewPipe(60)
	ctx, cancel := context.WithCancel(context.Background())
	run := &pumpRun{end: make(chan PumpEnd, 1), cancel: cancel, phone: phone, clk: newFakeClock(), rtp: r}
	r.clk = run.clk
	go func() { run.end <- RunPump(ctx, pumpSide, r, settle, gate, run.clk, nil) }()
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
	// The pump reacts to the timer asynchronously: keep reading (the fake clock does
	// not move) until the downlink turns into audio.
	var out []int16
	for i := 0; i < 2000 && out == nil; i++ {
		if pcm := toPCM(p.exchange(t, make([]byte, 60))); pcm[0] != 0 {
			out = pcm
		}
	}
	if out == nil {
		t.Fatal("downlink stayed silent after settle")
	}
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

func (r *fakeRTP) silenceTimeline() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.silenceTimes...)
}

func TestNoRTPSilenceWhileReadsFlow(t *testing.T) {
	r := newFakeRTP(0)
	p := startPump(t, 0, r)
	for i := 0; i < 100; i++ { // 375 ms of steady reads
		p.exchange(t, make([]byte, 60))
		p.clk.Advance(3750 * time.Microsecond)
	}
	if n := r.silenceCount(); n != 0 {
		t.Fatalf("WriteSilence called %d times while reads flowed", n)
	}
}

func TestStallThenResume(t *testing.T) {
	r := newFakeRTP(0)
	pushRamp(r.pl, 4000)
	p := startPump(t, 0, r)
	var writes atomic.Int64
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := p.phone.Read(buf); err != nil {
				return
			}
			writes.Add(1)
		}
	}()
	for i := 0; i < 6; i++ {
		p.clk.Advance(100 * time.Millisecond)
	}
	waitFor(t, "stall writes", func() bool { return writes.Load() > 50 })
	waitFor(t, "stall silences", func() bool { return len(r.silenceTimeline()) >= 15 })
	ts := r.silenceTimeline()
	if len(ts) < 10 {
		t.Fatalf("only %d silences during a 600 ms stall", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		// The pump may observe the fake clock a step late, hence the tolerance.
		if d := ts[i].Sub(ts[i-1]); d < 16*time.Millisecond || d > 24*time.Millisecond {
			t.Fatalf("silence %d spaced %v from the previous, want 20ms", i, d)
		}
	}

	// Reads resume: silences and stall writes stop; exactly one write per read.
	silences := r.silenceCount()
	time.Sleep(10 * time.Millisecond) // let in-flight stall writes land
	base := writes.Load()
	const reads = 60
	for i := 0; i < reads; i++ {
		if _, err := p.phone.Write(make([]byte, 60)); err != nil {
			t.Fatal(err)
		}
		want := base + int64(i) + 1
		waitFor(t, "downlink answer", func() bool { return writes.Load() >= want })
		p.clk.Advance(3750 * time.Microsecond)
	}
	time.Sleep(10 * time.Millisecond)
	if got := writes.Load() - base; got != reads {
		t.Fatalf("%d SCO writes for %d reads after resume", got, reads)
	}
	if n := r.silenceCount(); n > silences+1 {
		t.Fatalf("silences grew from %d to %d after reads resumed", silences, n)
	}
}

func TestStartupEndedOnce(t *testing.T) {
	// Leaving startup mode re-primes the playout to the jitter target, so with less
	// than the target buffered a playout that (again) left startup mode is silent,
	// while one in startup mode plays what it has.
	r := newFakeRTP(600)
	pushRamp(r.pl, 12000) // 1.5 s
	p := startPump(t, 100*time.Millisecond, r)
	p.clk.Advance(150 * time.Millisecond)
	// The pump reacts to the timer asynchronously: read until audio plays.
	audible := false
	for i := 0; i < 2000 && !audible; i++ {
		audible = toPCM(p.exchange(t, make([]byte, 60)))[0] != 0
	}
	if !audible {
		t.Fatal("pump never ended startup mode")
	}
	for r.pl.Depth() > 500*time.Millisecond {
		p.exchange(t, make([]byte, 60))
	}
	r.pl.SetStartup(true) // put it back; the pump must not end startup again
	for i := 0; i < 50; i++ {
		if toPCM(p.exchange(t, make([]byte, 60)))[0] == 0 {
			t.Fatal("pump ended startup mode a second time")
		}
		p.clk.Advance(3750 * time.Microsecond)
	}
}

func TestTransientRTPErrorsSurvive(t *testing.T) {
	r := newFakeRTP(0)
	r.frameErr = func(a int) error {
		if a <= 4 {
			return errors.New("send: no buffer space")
		}
		return nil
	}
	p := startPump(t, 0, r)
	for i := 0; i < 100; i++ {
		p.exchange(t, make([]byte, 60)) // the downlink is answered despite failed uplink sends
		p.clk.Advance(3750 * time.Microsecond)
	}
	select {
	case end := <-p.end:
		t.Fatalf("pump ended: %q", end)
	default:
	}
	if r.frameCount() < 10 {
		t.Fatalf("only %d frames sent after the errors cleared", r.frameCount())
	}
}

func TestPersistentRTPErrorsEnd(t *testing.T) {
	r := newFakeRTP(0)
	r.frameErr = func(int) error { return errors.New("send: network unreachable") }
	p := startPump(t, 0, r)
	stop := p.drain()
	defer stop()
	start := p.clk.Now()
	var end PumpEnd
	for i := 0; i < 400 && end == ""; i++ {
		p.phone.Write(make([]byte, 60))
		p.clk.Advance(50 * time.Millisecond)
		time.Sleep(time.Millisecond)
		select {
		case end = <-p.end:
		default:
		}
	}
	if end != PumpRTPError {
		t.Fatalf("end = %q, want %q", end, PumpRTPError)
	}
	if el := p.clk.Now().Sub(start); el < 5*time.Second || el > 6*time.Second {
		t.Fatalf("ended after %v, want about 5s", el)
	}
}

func TestPermanentRTPErrorEndsAtOnce(t *testing.T) {
	for name, err := range map[string]error{
		"closed":      fmt.Errorf("rtp: send: %w", net.ErrClosed),
		"not running": rtp.ErrNotRunning,
	} {
		t.Run(name, func(t *testing.T) {
			r := newFakeRTP(0)
			r.frameErr = func(int) error { return err }
			p := startPump(t, 0, r)
			for i := 0; i < 6; i++ {
				p.phone.Write(make([]byte, 60))
			}
			select {
			case end := <-p.end:
				if end != PumpRTPError {
					t.Fatalf("end = %q", end)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("pump did not return")
			}
			if p.clk.Now().After(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)) {
				t.Fatal("clock moved")
			}
		})
	}
}

// blockedLink never completes a Read or a Write, like a wedged SCO socket.
type blockedLink struct{ block chan struct{} }

func (b blockedLink) Read([]byte) (int, error)  { <-b.block; return 0, io.EOF }
func (b blockedLink) Write([]byte) (int, error) { <-b.block; return 0, io.EOF }
func (b blockedLink) Close() error              { return nil } // does not unblock
func (b blockedLink) MTU() int                  { return 60 }
func (b blockedLink) Remote() string            { return "11:11:11:11:11:11" }

func TestBlockedLinkStalls(t *testing.T) {
	clk := newFakeClock()
	r := newFakeRTP(0)
	b := blockedLink{block: make(chan struct{})}
	t.Cleanup(func() { close(b.block) })
	end := make(chan PumpEnd, 1)
	go func() { end <- RunPump(context.Background(), b, r, 0, nil, clk, nil) }()
	waitFor(t, "pump timers", func() bool { return clk.armed() >= 3 })
	var got PumpEnd
	for i := 0; i < 400 && got == ""; i++ {
		clk.Advance(50 * time.Millisecond)
		select {
		case got = <-end:
		case <-time.After(time.Millisecond):
		}
	}
	if got != PumpSCOStall {
		t.Fatalf("end = %q, want %q", got, PumpSCOStall)
	}
}

func TestPumpPanicBecomesEnd(t *testing.T) {
	r := newFakeRTP(0)
	r.frameErr = func(int) error { panic("rtp exploded") }
	p := startPump(t, 0, r)
	for i := 0; i < 6; i++ {
		p.phone.Write(make([]byte, 60))
	}
	select {
	case end := <-p.end:
		if !strings.HasPrefix(string(end), "panic: ") || !strings.Contains(string(end), "rtp exploded") {
			t.Fatalf("end = %q", end)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return")
	}
}

func sine(n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	return out
}

// The far end's voice played to the phone and returned 10 dB quieter (line echo)
// is sent to the far end as silence; caller speech louder than it gets through.
func TestUplinkEchoGated(t *testing.T) {
	r := newFakeRTP(0)
	played := sine(8000, 0.3)
	for off := 0; off < len(played); off += 160 {
		r.pl.Push(uint32(off), 1, played[off:off+160])
	}
	p := startPumpGate(t, 0, r, echo.NewGate(6, 600))
	for i := 0; i < 16; i++ { // the echo arrives 60 ms late: three frames of silence
		p.exchange(t, toBytes(make([]int16, 30)))
	}
	echoed := sine(30*16*4, 0.3*0.316) // -10 dB
	for i := 0; i < 16*4; i++ {
		p.exchange(t, toBytes(echoed[i*30:(i+1)*30]))
	}
	if r.frameCount() != 15 {
		t.Fatalf("frames = %d", r.frameCount())
	}
	for i := 3; i < 15; i++ {
		for _, s := range r.frame(i) {
			if s != 0 {
				t.Fatalf("echo frame %d reached the far end", i)
			}
		}
	}
	loud := sine(30*16*5, 0.9) // +9.5 dB over the played audio
	for i := 0; i < 16*5; i++ {
		p.exchange(t, toBytes(loud[i*30:(i+1)*30]))
	}
	passed := 0
	for i := 15; i < r.frameCount(); i++ {
		for _, s := range r.frame(i) {
			if s != 0 {
				passed++
				break
			}
		}
	}
	if passed < 13 { // two-block onset
		t.Fatalf("caller speech frames passed = %d of %d", passed, r.frameCount()-15)
	}
}
