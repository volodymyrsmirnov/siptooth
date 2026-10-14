package bridge

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// bridge answers the call and brings SCO up; it returns the SIP call and the
// phone's end of the SCO link.
func (r *rig) bridge() (*fakeCall, sco.Link) {
	r.t.Helper()
	fc := r.answer()
	return fc, r.connect()
}

// feed plays the phone: it sends audio frames and discards what the pump
// writes, so the pump never counts a stall while a test moves the clock.
func feed(t *testing.T, phone sco.Link) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); phone.Close() })
	go func() {
		buf := make([]byte, phone.MTU())
		for {
			if _, err := phone.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() {
		frame := make([]byte, phone.MTU())
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if _, err := phone.Write(frame); err != nil {
				return
			}
		}
	}()
}

// discard only reads what the pump writes.
func discard(t *testing.T, phone sco.Link) {
	t.Helper()
	t.Cleanup(func() { phone.Close() })
	go func() {
		buf := make([]byte, phone.MTU())
		for {
			if _, err := phone.Read(buf); err != nil {
				return
			}
		}
	}()
}

func noMediaTimeout(c *config.Config) { c.Calls.MediaTimeoutSeconds = 0 }

func TestCallerHangsUp(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	r.clock.Advance(3 * time.Second) // ringing
	r.connect()
	r.clock.Advance(2 * time.Second) // bridged
	r.tel.send(hfp.CallEnded, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
	// ringing_seconds stops when the phone call is answered, bridged_seconds
	// runs from SCO up.
	if e.RingingSeconds != 3 {
		t.Errorf("ringing_seconds = %v, want 3", e.RingingSeconds)
	}
	if e.BridgedSeconds == nil || *e.BridgedSeconds != 2 {
		t.Errorf("bridged_seconds = %v, want 2", e.BridgedSeconds)
	}
	if e.Media == nil {
		t.Error("no media stats")
	}
	if st := r.c.Status(); st.State != "idle" || st.LastOutcome != OutcomeCompleted {
		t.Errorf("status = %+v", st)
	}
	if r.sip.media(0).closeCount() != 1 {
		t.Error("media not closed")
	}
	line := ""
	for _, l := range strings.Split(r.log.String(), "\n") {
		if strings.Contains(l, `msg="call ended"`) {
			line = l
		}
	}
	for _, want := range []string{"outcome=completed", "hangup_by=caller", "ringing_s=3", "bridged_s=2", "codec=PCMU"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary lacks %q: %s", want, line)
		}
	}
}

func TestBridgedSecondsNullWhenNeverBridged(t *testing.T) {
	r := newRig(t, nil).start()
	r.answer()
	r.tel.send(hfp.CallEnded, "")
	if e := r.waitEnded(); e.BridgedSeconds != nil {
		t.Errorf("bridged_seconds = %v", *e.BridgedSeconds)
	}
}

func TestFarEndBye(t *testing.T) {
	r := newRig(t, nil).start()
	fc, _ := r.bridge()
	fc.ended <- sip.RemoteBye // nothing buffered: no wait
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted || e.HangupBy != HangupFarEnd {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 1)
}

func pushAudio(m *fakeMedia, samples int) {
	m.pl.Push(0, 1, make([]int16, samples))
}

func TestFarEndByeDrainsDownlink(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	m := r.sip.media(0)
	pushAudio(m, 3200) // 400 ms
	a, phone := sco.NewPipe(60)
	r.callActive()
	r.lis.links <- a
	r.waitState("bridged")
	discard(t, phone)
	t0 := r.clock.Now()
	fc.ended <- sip.RemoteBye
	r.settle()
	_, h := r.tel.counts()
	if h != 0 {
		t.Fatal("hung up the phone before the audio played out")
	}
	for i := 0; i < 40 && len(r.hooks.named("call.ended")) == 0; i++ {
		r.clock.Advance(100 * time.Millisecond)
		r.settle()
	}
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted || e.HangupBy != HangupFarEnd {
		t.Errorf("ended = %+v", e)
	}
	if took := r.clock.Now().Sub(t0); took >= 3*time.Second || took < 800*time.Millisecond {
		t.Errorf("drain took %v, want the playout time (settle 800 ms + 400 ms)", took)
	}
	r.wantHangups(fc, 0, 1)
}

func TestFarEndByeDrainLimit(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	pushAudio(r.sip.media(0), 40000) // 5 s, never played out: nobody reads the SCO link
	a, _ := sco.NewPipe(60)
	r.callActive()
	r.lis.links <- a
	r.waitState("bridged")
	fc.ended <- sip.RemoteBye
	r.waitFor("drain timer", func() bool { return r.clock.hasTimer(3 * time.Second) })
	r.clock.Advance(2999 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatal("ended before the drain limit")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted || e.HangupBy != HangupFarEnd {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 1)
}

func TestTransportLost(t *testing.T) {
	r := newRig(t, nil).start()
	fc, _ := r.bridge()
	fc.ended <- sip.TransportLost
	e := r.waitEnded()
	if e.Outcome != OutcomeSignalingLost || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 1)
}

func TestMediaTimeout(t *testing.T) {
	r := newRig(t, nil).start()
	fc, phone := r.bridge()
	feed(t, phone)
	r.clock.Advance(9 * time.Second)
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatalf("state %s: ended before the timeout", r.c.Status().State)
	}
	r.clock.Advance(2 * time.Second)
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaTimeout || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestMediaTimeoutResetByRx(t *testing.T) {
	r := newRig(t, nil).start()
	_, phone := r.bridge()
	feed(t, phone)
	m := r.sip.media(0)
	for i := 0; i < 15; i++ { // 15 s, RTP keeps arriving
		m.setLastRx(r.clock.Now())
		r.clock.Advance(time.Second)
		r.settle()
	}
	if r.c.Status().State != "bridged" {
		t.Fatalf("state %s: RTP was flowing", r.c.Status().State)
	}
}

func TestSCOLossNoCHUP(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc, phone := r.bridge()
	phone.Close() // SCO EOF
	r.waitFor("grace timer", func() bool { return r.clock.hasTimer(1500 * time.Millisecond) })
	r.clock.Advance(1499 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatal("ended inside the grace period")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeAudioLost || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0) // the cellular call may continue on the handset
}

func TestSCOStallNoCHUP(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc, phone := r.bridge() // the phone never sends a frame
	discard(t, phone)
	r.waitFor("stall", func() bool { // detected after 5 s without a read
		r.clock.Advance(250 * time.Millisecond)
		r.settle()
		return strings.Contains(r.log.String(), "SCO lost")
	})
	r.clock.Advance(1500 * time.Millisecond) // the grace timer was armed during the last step
	e := r.waitEnded()
	if e.Outcome != OutcomeAudioLost {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

func TestSCOLossThenCallEnded(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc, phone := r.bridge()
	phone.Close()
	r.waitFor("grace timer", func() bool { return r.clock.hasTimer(1500 * time.Millisecond) })
	r.tel.send(hfp.CallEnded, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.clock.Advance(2 * time.Second) // the grace timer is dead
	r.wantHangups(fc, 1, 0)
}

func TestSCOLossThenFarEndBye(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc, phone := r.bridge()
	phone.Close()
	r.waitFor("grace timer", func() bool { return r.clock.hasTimer(1500 * time.Millisecond) })
	fc.ended <- sip.RemoteBye
	r.settle()
	r.clock.Advance(1500 * time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeAudioLost {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 0) // the far end already hung up, the phone is left alone
}

func TestMaxDuration(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Calls.MaxDurationSeconds = 3; c.Calls.MediaTimeoutSeconds = 0 }).start()
	fc, phone := r.bridge()
	feed(t, phone)
	r.waitFor("max timer", func() bool { return r.clock.hasTimer(3 * time.Second) })
	r.clock.Advance(2999 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatal("ended early")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeMaxDuration || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestMaxDurationOffByDefault(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	_, phone := r.bridge()
	feed(t, phone)
	r.clock.Advance(20 * time.Second)
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatalf("state %s", r.c.Status().State)
	}
}

func TestDisconnectedWhileBridged(t *testing.T) {
	r := newRig(t, nil).start()
	fc, _ := r.bridge()
	r.tel.send(hfp.Disconnected, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeBluetoothLost || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

func TestShutdownWhileBridged(t *testing.T) {
	r := newRig(t, nil).start()
	fc, _ := r.bridge()
	r.cancel()
	<-r.done
	r.done <- nil
	if _, h := r.tel.counts(); fc.hangups() != 1 || h != 1 {
		t.Errorf("BYE=%d AT+CHUP=%d after Run returned", fc.hangups(), h)
	}
	e := r.hooks.named("call.ended")
	if len(e) != 1 || e[0].Outcome != OutcomeShutdown || e[0].HangupBy != HangupSiptooth || e[0].BridgedSeconds == nil {
		t.Errorf("ended = %+v", e)
	}
}

func TestRTPSendFailureEndsCall(t *testing.T) {
	r := newRig(t, nil).start()
	fc, phone := r.bridge()
	m := r.sip.media(0)
	m.mu.Lock()
	m.writeErr = rtp.ErrNotRunning
	m.mu.Unlock()
	feed(t, phone)
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestMediaErrorTearsDown(t *testing.T) {
	r := newRig(t, nil).start()
	fc, _ := r.bridge()
	r.sip.media(0).errCh <- errBoom
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || e.HangupBy != HangupSiptooth || !strings.Contains(e.Error, "boom") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestMediaUpdateApplied(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer() // already during Answering
	want := netip.MustParseAddrPort("198.51.100.7:5004")
	fc.updates <- sdp.Media{Addr: want}
	m := r.sip.media(0)
	r.waitFor("update", func() bool { return m.updateCount() == 1 })
	r.connect()
	want2 := netip.MustParseAddrPort("198.51.100.8:5004")
	fc.updates <- sdp.Media{Addr: want2}
	r.waitFor("second update", func() bool { return m.updateCount() == 2 })
	if m.updates[0].Addr != want || m.updates[1].Addr != want2 {
		t.Errorf("updates = %v", m.updates)
	}
}

func TestRingWhileBridgedIgnored(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	r.bridge()
	r.tel.send(hfp.Ring, "") // call waiting
	r.tel.send(hfp.CallSetupCleared, "")
	r.tel.send(hfp.CallActive, "")
	r.settle()
	if st := r.c.Status(); st.State != "bridged" || r.sip.starts() != 1 {
		t.Errorf("status %+v, INVITEs %d", st, r.sip.starts())
	}
}

func TestPanicRecovered(t *testing.T) {
	r := newRig(t, nil)
	r.sip.panicFirstStats = true
	r.start()
	fc, _ := r.bridge()
	r.clock.Advance(time.Second) // the media poll calls Stats, which panics
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || !strings.Contains(e.Error, "stats exploded") || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1) // both legs, the call was ours
	if st := r.c.Status(); st.State != "idle" {
		t.Errorf("status = %+v", st)
	}

	// The coordinator keeps running: the next ring dials normally.
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380509999999")
	r.waitFor("second INVITE", func() bool { return r.sip.starts() == 2 })
	if got := r.hooks.named("call.ringing"); len(got) != 2 {
		t.Errorf("ringing x%d", len(got))
	}
}

func TestPanicRecoveredBeforeCallActiveLeavesPhone(t *testing.T) {
	r := newRig(t, nil)
	r.sip.panicFirstStats = true
	r.start()
	fc := r.answer()
	r.clock.Advance(time.Second)
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0) // not answered yet: the phone is not ours
}

func TestGoroutinePanicRecovered(t *testing.T) {
	r := newRig(t, nil)
	r.sip.panicSilenceFirst = true
	r.start()
	fc := r.answer()
	r.waitFor("silence tick", func() bool { r.clock.Advance(20 * time.Millisecond); return len(r.hooks.named("call.ended")) > 0 })
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || !strings.Contains(e.Error, "silence exploded") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+1")
	r.waitFor("second INVITE", func() bool { return r.sip.starts() == 2 })
}

func TestWithheldCaller(t *testing.T) {
	r := newRig(t, nil).start()
	r.tel.send(hfp.Ring, "")
	r.waitArmed(1)
	r.clock.Advance(time.Duration(r.cfg.Calls.CallerIDWaitMs) * time.Millisecond) // no +CLIP
	r.waitFor("INVITE", func() bool { return r.sip.starts() == 1 })
	fc := newFakeCall()
	r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
	r.waitState("answering")
	r.connect()
	r.tel.send(hfp.CallEnded, "")
	r.waitEnded()

	if r.sip.req(0).From != "sip:anonymous@sip.example.com" {
		t.Errorf("From = %q", r.sip.req(0).From)
	}
	for _, ev := range []string{"call.ringing", "call.connected", "call.ended"} {
		infos := r.hooks.named(ev)
		if len(infos) != 1 {
			t.Fatalf("%s x%d", ev, len(infos))
		}
		raw, err := hooks.BuildPayload(ev, infos[0], false)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Call map[string]any `json:"call"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if c, ok := p.Call["caller"]; !ok || c != nil || p.Call["caller_withheld"] != true {
			t.Errorf("%s payload call = %v", ev, p.Call)
		}
	}
	if !strings.Contains(r.log.String(), "caller=withheld") {
		t.Error("summary line does not say withheld")
	}
}
