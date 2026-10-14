package bridge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// answer rings the phone, dials and lets the SIP side answer; it returns once
// ATA went out.
func (r *rig) answer() *fakeCall {
	r.t.Helper()
	fc := r.answerAny()
	r.waitState("answering")
	return fc
}

// answerAny is answer for tests where the ATA result may end the call at once.
func (r *rig) answerAny() *fakeCall {
	r.t.Helper()
	r.ringAndDial("+380501234567")
	fc := newFakeCall()
	r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
	r.waitFor("ATA", func() bool { a, _ := r.tel.counts(); return a == 1 })
	return fc
}

// callActive reports CallActive and waits for the SCO timers.
func (r *rig) callActive() {
	r.t.Helper()
	r.tel.send(hfp.CallActive, "")
	r.waitFor("SCO timers", func() bool { return r.clock.hasTimer(2 * time.Second) })
}

// connect reports CallActive and offers an SCO link; it returns the phone's end.
func (r *rig) connect() sco.Link {
	r.t.Helper()
	r.callActive()
	a, b := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
	return b
}

func (r *rig) wantHangups(fc *fakeCall, byes, chups int) {
	r.t.Helper()
	r.waitFor("hangups", func() bool {
		_, h := r.tel.counts()
		return fc.hangups() >= byes && h >= chups
	})
	r.settle()
	_, h := r.tel.counts()
	if fc.hangups() != byes || h != chups {
		r.t.Errorf("BYE=%d AT+CHUP=%d, want %d and %d", fc.hangups(), h, byes, chups)
	}
}

func TestAnsweredThenATAThenSCO(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	m := r.sip.media(0)
	if m.startCount() != 1 || m.started[0].Addr != fc.Remote().Addr {
		t.Errorf("media started %v, want remote %v", m.started, fc.Remote())
	}
	// Silence flows from the answer on, before SCO exists.
	r.waitFor("silence", func() bool { r.clock.Advance(20 * time.Millisecond); return m.silenceCount() >= 3 })
	if len(r.hooks.named("call.connected")) != 0 {
		t.Fatal("call.connected before SCO")
	}

	r.tel.send(hfp.CallActive, "")
	r.settle()
	if len(r.hooks.named("call.connected")) != 0 || r.c.Status().State != "answering" {
		t.Fatal("bridged before the SCO link")
	}
	a, _ := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
	conn := r.hooks.named("call.connected")
	if len(conn) != 1 {
		t.Fatalf("hooks %v", r.hooks.names())
	}
	c := conn[0]
	if c.SIPCallID != "sip-call-1" || c.Codec != "PCMU" || c.RemoteMedia != "203.0.113.9:4000" || c.ConnectedAt == nil || c.NATMode != "local" {
		t.Errorf("connected = %+v", c)
	}
	if got := r.hooks.names(); got[len(got)-1] != "call.connected" {
		t.Errorf("hooks %v", got)
	}
	if _, h := r.tel.counts(); h != 0 || fc.hangups() != 0 {
		t.Error("hung up")
	}
}

func TestSRTPKeyPassedToMediaStart(t *testing.T) {
	r := newRig(t, nil)
	r.sip.mu.Lock()
	r.sip.localKey = []byte("0123456789abcdef0123456789ab")
	r.sip.mu.Unlock()
	r.start()
	r.answer()
	m := r.sip.media(0)
	if string(m.keys[0]) != "0123456789abcdef0123456789ab" {
		t.Errorf("key = %q", m.keys[0])
	}
}

func TestMediaStartFailure(t *testing.T) {
	r := newRig(t, nil)
	r.sip.mediaStartErr = errBoom
	r.start()
	r.ringAndDial("+380501234567")
	fc := newFakeCall()
	r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || !strings.Contains(e.Error, "boom") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
	if a, _ := r.tel.counts(); a != 0 {
		t.Error("ATA sent without media")
	}
}

func TestATAErrorByesImmediately(t *testing.T) {
	r := newRig(t, nil)
	r.tel.setAnswerErr(hfp.ErrResponse)
	r.start()
	fc := r.answerAny()
	e := r.waitEnded() // no clock advance: no waiting
	if e.Outcome != OutcomeAnswerFailed || e.HangupBy != HangupSiptooth || !strings.Contains(e.Error, "ATA") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

// ATA definitely failed (ERROR, command never sent): a CallActive afterwards
// is the owner picking up on the handset, never ours.
func TestOwnerAnswersAfterATAError(t *testing.T) {
	for _, err := range []error{hfp.ErrResponse, hfp.ErrNotSent, hfp.ErrClosed, errBoom} {
		t.Run(fmt.Sprint(err), func(t *testing.T) {
			r := newRig(t, nil)
			r.tel.setAnswerErr(err)
			r.start()
			fc := r.answerAny()
			r.waitEnded()
			r.tel.send(hfp.CallActive, "")
			r.clock.Advance(10 * time.Second)
			r.wantHangups(fc, 1, 0)
		})
	}
}

func TestATANotSentIsDefinite(t *testing.T) {
	r := newRig(t, nil)
	r.tel.setAnswerErr(fmt.Errorf("wrapped: %w", hfp.ErrNotSent)) // matches ErrTimeout too
	r.start()
	fc := r.answerAny()
	e := r.waitEnded()
	if e.Outcome != OutcomeAnswerFailed {
		t.Errorf("ended = %+v", e)
	}
	r.tel.send(hfp.CallActive, "")
	r.wantHangups(fc, 1, 0)
}

func TestATATimeoutThenCallActive(t *testing.T) {
	r := newRig(t, nil)
	r.tel.setAnswerErr(hfp.ErrTimeout) // sent, no final result: ambiguous
	r.start()
	r.answer()
	r.waitFor("CallActive timer", func() bool { return r.clock.hasTimer(5 * time.Second) })
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("ambiguous ATA ended the call")
	}
	r.tel.send(hfp.CallActive, "")
	a, _ := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
	r.clock.Advance(5 * time.Second) // the CallActive timer is gone
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatal("stale CallActive timer ended the call")
	}
}

func TestATATimeoutNoCallActive(t *testing.T) {
	r := newRig(t, nil)
	r.tel.setAnswerErr(hfp.ErrTimeout)
	r.start()
	fc := r.answer()
	r.waitFor("CallActive timer", func() bool { return r.clock.hasTimer(5 * time.Second) })
	r.clock.Advance(4999 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("ended early")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeAnswerFailed || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0) // BYE; the phone is left alone
}

func TestATAOKWaitsForCallActive(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.waitFor("CallActive timer", func() bool { return r.clock.hasTimer(5 * time.Second) })
	r.clock.Advance(5 * time.Second)
	e := r.waitEnded()
	if e.Outcome != OutcomeAnswerFailed {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

// CallActive can race ahead of the ATA result (and the result may even be an
// error): the indicator decides.
func TestCallActiveBeforeATAResult(t *testing.T) {
	r := newRig(t, nil)
	r.tel.gate = make(chan struct{})
	r.tel.setAnswerErr(hfp.ErrResponse)
	r.start()
	r.answer()
	r.callActive()
	close(r.tel.gate)
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatalf("state %s: an ATA error after CallActive must not end the call", r.c.Status().State)
	}
	a, _ := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
}

func TestNoSCOChupAndBye(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.callActive()
	r.clock.Advance(4999 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("ended before 5 s")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeAnswerFailed || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestSCODialFallbackAfter2s(t *testing.T) {
	r := newRig(t, nil).start()
	r.answer()
	r.callActive()
	if r.lis.acceptCount() != 1 {
		t.Errorf("accepts = %d", r.lis.acceptCount())
	}
	r.clock.Advance(1999 * time.Millisecond)
	r.settle()
	if len(r.dialer.dialed()) != 0 {
		t.Fatal("dialed before 2 s")
	}
	r.clock.Advance(time.Millisecond)
	r.waitFor("dial", func() bool { return len(r.dialer.dialed()) == 1 })
	if got := r.dialer.dialed()[0]; got != "AA:BB:CC:DD:EE:FF>11:11:11:11:11:11" {
		t.Errorf("dialed %q", got)
	}
	a, _ := sco.NewPipe(60)
	r.dialer.links <- a
	r.waitState("bridged")
	if len(r.hooks.named("call.connected")) != 1 {
		t.Error("no call.connected")
	}
}

func TestSCOBacklogDrained(t *testing.T) {
	r := newRig(t, nil).start()
	r.answer()
	r.callActive()
	a, b := sco.NewPipe(60)
	for i := 0; i < 16; i++ { // ringtone queued while the phone rang
		if _, err := b.Write(make([]byte, 60)); err != nil {
			t.Fatal(err)
		}
	}
	r.lis.links <- a
	r.waitState("bridged")
	r.settle()
	if n := r.sip.media(0).frameCount(); n != 0 {
		t.Errorf("%d frames sent from the backlog", n)
	}
}

func TestSCOFromUnexpectedDeviceIgnored(t *testing.T) {
	r := newRig(t, nil).start()
	r.answer()
	r.callActive()
	_, stranger := sco.NewPipe(60) // remote 22:22:..., not the phone
	r.lis.links <- stranger
	r.waitFor("listening again", func() bool { return r.lis.acceptCount() == 2 })
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("accepted a stranger")
	}
	a, _ := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
}

func TestFarEndByeBeforeCallActive(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	e := r.waitEnded()
	if e.Outcome != OutcomeFarEndEnded || e.HangupBy != HangupFarEnd {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 0) // the phone rings on
	// Our ATA answered after all: the caller must not be left in silence.
	r.tel.send(hfp.CallActive, "")
	r.waitFor("AT+CHUP", func() bool { _, h := r.tel.counts(); return h == 1 })
	r.settle()
	if _, h := r.tel.counts(); h != 1 || len(r.hooks.named("call.ended")) != 1 {
		t.Errorf("hangups %d, ended %d", h, len(r.hooks.named("call.ended")))
	}
	// Once only.
	r.tel.send(hfp.CallActive, "")
	r.settle()
	if _, h := r.tel.counts(); h != 1 {
		t.Errorf("hangups = %d", h)
	}
}

func TestLateCallActiveWindowCloses(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.ended <- sip.TransportLost
	r.waitEnded()
	r.waitFor("window timer", func() bool { return r.clock.hasTimer(5 * time.Second) })
	r.clock.Advance(5 * time.Second)
	r.settle()
	r.tel.send(hfp.CallActive, "") // the owner, much later
	r.wantHangups(fc, 0, 0)
}

func TestLateCallActiveWindowEndsOnNewCallOrCleared(t *testing.T) {
	for name, ev := range map[string]hfp.EventKind{"setup cleared": hfp.CallSetupCleared, "call ended": hfp.CallEnded} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil).start()
			fc := r.answer()
			fc.ended <- sip.RemoteBye
			r.waitEnded()
			r.tel.send(ev, "")
			r.tel.send(hfp.CallActive, "")
			r.wantHangups(fc, 0, 0)
		})
	}
}

// ATA still pending when the far end hangs up; a definite failure arriving
// later means the phone was never answered.
func TestLateATAFailureClosesWindow(t *testing.T) {
	r := newRig(t, nil)
	r.tel.gate = make(chan struct{})
	r.tel.setAnswerErr(hfp.ErrResponse)
	r.start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	close(r.tel.gate)
	r.settle()
	r.tel.send(hfp.CallActive, "")
	r.wantHangups(fc, 0, 0)
}

func TestFarEndByeAfterCallActive(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.callActive()
	fc.ended <- sip.RemoteBye
	e := r.waitEnded()
	if e.Outcome != OutcomeFarEndEnded || e.HangupBy != HangupFarEnd {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 0, 1)
}

func TestCallerAbandonedBeforeCallActive(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeCallerAbandoned || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

func TestCallEndedAfterCallActiveBeforeSCO(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.callActive()
	r.tel.send(hfp.CallEnded, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeCallerAbandoned || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0) // the phone call is already over
}

func TestDisconnectedDuringAnswering(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("callActive=", active), func(t *testing.T) {
			r := newRig(t, nil).start()
			fc := r.answer()
			if active {
				r.callActive()
			}
			r.tel.send(hfp.Disconnected, "")
			e := r.waitEnded()
			if e.Outcome != OutcomeBluetoothLost || e.HangupBy != HangupSiptooth {
				t.Errorf("ended = %+v", e)
			}
			r.wantHangups(fc, 1, 0)
		})
	}
}

func TestShutdownDuringAnswering(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("callActive=", active), func(t *testing.T) {
			r := newRig(t, nil).start()
			fc := r.answer()
			if active {
				r.callActive()
			}
			r.cancel()
			<-r.done
			r.done <- nil
			want := 0
			if active {
				want = 1
			}
			// Run waited for the background hangups.
			if _, h := r.tel.counts(); fc.hangups() != 1 || h != want {
				t.Errorf("BYE=%d AT+CHUP=%d, want 1 and %d", fc.hangups(), h, want)
			}
			e := r.hooks.named("call.ended")
			if len(e) != 1 || e[0].Outcome != OutcomeShutdown || e[0].HangupBy != HangupSiptooth {
				t.Errorf("ended = %+v", e)
			}
		})
	}
}

// A SIP answer that is still in flight when the daemon stops is hung up
// before Run returns, whichever way it races with the shutdown.
func TestShutdownWithPendingAnswerHangsUp(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := newRig(t, nil).start()
		r.ringAndDial("+380501234567")
		fc := newFakeCall()
		r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
		r.cancel()
		<-r.done
		r.done <- nil
		if fc.hangups() != 1 {
			t.Fatalf("run %d: BYEs = %d, want 1", i, fc.hangups())
		}
	}
}

func TestMediaTimeoutWhileAnswering(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Calls.MediaTimeoutSeconds = 3 }).start()
	fc := r.answer()
	r.callActive()
	for i := 0; i < 3; i++ {
		r.clock.Advance(time.Second)
		r.settle()
	}
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaTimeout || e.HangupBy != HangupSiptooth {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

func TestMediaErrorWhileAnswering(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.sip.media(0).errCh <- errors.New("socket closed")
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || !strings.Contains(e.Error, "socket closed") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0) // not answered yet: the phone is left alone
}
