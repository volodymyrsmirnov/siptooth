package bridge

import (
	"strings"
	"sync"
	"testing"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// A CallActive after teardown is ours only if the ATA result is ok or ambiguous.
func TestLateWindowWaitsForATAResult(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		chups int
	}{
		"ok":        {nil, 1},
		"ambiguous": {hfp.ErrTimeout, 1},
		"not sent":  {hfp.ErrNotSent, 0},
		"rejected":  {hfp.ErrResponse, 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.tel.gate, r.tel.ignoreCtx = make(chan struct{}), true
			release := sync.OnceFunc(func() { close(r.tel.gate) })
			t.Cleanup(release)
			r.tel.setAnswerErr(tc.err)
			r.start()
			fc := r.answer()
			fc.ended <- sip.RemoteBye
			r.waitEnded()
			r.tel.send(hfp.CallActive, "")
			r.settle()
			if _, h := r.tel.counts(); h != 0 {
				t.Fatal("AT+CHUP before the ATA result was known")
			}
			release()
			r.wantHangups(fc, 0, tc.chups)
		})
	}
}

// CallActive seen, then the SIP side ends while the ATA result is pending.
func TestTeardownWithCallActiveWatchesPendingATA(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		chups int
	}{"ok": {nil, 1}, "ambiguous": {hfp.ErrTimeout, 1}, "rejected": {hfp.ErrResponse, 0}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.tel.gate, r.tel.ignoreCtx = make(chan struct{}), true
			release := sync.OnceFunc(func() { close(r.tel.gate) })
			t.Cleanup(release)
			r.tel.setAnswerErr(tc.err)
			r.start()
			fc := r.answer()
			r.callActive()
			fc.ended <- sip.RemoteBye
			r.waitEnded()
			r.settle()
			if _, h := r.tel.counts(); h != 0 {
				t.Fatal("AT+CHUP before the ATA result was known")
			}
			release()
			r.wantHangups(fc, 0, tc.chups)
		})
	}
}

// A new ring while the old call only waits out its CallActive window.
func TestRingEndsCallWaitingAfterSetupCleared(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "")
	r.settle()
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380509999999")
	r.waitFor("second INVITE", func() bool { return r.sip.starts() == 2 })
	if from := r.sip.req(1).From; !strings.Contains(from, "380509999999") {
		t.Errorf("second call From = %q, want the new caller ID", from)
	}
	ended := r.hooks.named("call.ended")
	if len(ended) != 1 || ended[0].ID != "call-1" || ended[0].Outcome != OutcomeCallerAbandoned || ended[0].HangupBy != HangupCaller {
		t.Fatalf("ended = %+v", ended)
	}
	// The owner picks up the new call: it is not attributed to the old ATA.
	r.tel.send(hfp.CallActive, "")
	r.waitFor("second call ended", func() bool { return len(r.hooks.named("call.ended")) == 2 })
	if e := r.hooks.named("call.ended")[1]; e.ID != "call-2" || e.Outcome != OutcomeOwnerAnswered {
		t.Errorf("second call ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

// A panic inside the pump is contained and reported as media_error.
func TestPumpPanicIsMediaError(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc, phone := r.bridge()
	m := r.sip.media(0)
	m.mu.Lock()
	m.panicFrame = true
	m.mu.Unlock()
	feed(t, phone)
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaError || !strings.Contains(e.Error, "frame exploded") {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 1)
}

// The cellular call is confirmed over: a late ATA result must not send CHUP,
// which could hit a later outgoing call of the owner.
func TestNoLateWindowAfterCallEnded(t *testing.T) {
	r := newRig(t, noMediaTimeout)
	r.tel.gate, r.tel.ignoreCtx = make(chan struct{}), true
	release := sync.OnceFunc(func() { close(r.tel.gate) })
	t.Cleanup(release)
	r.start()
	fc, _ := r.bridge()
	r.tel.send(hfp.CallEnded, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeCompleted {
		t.Fatalf("ended = %+v", e)
	}
	release() // the ATA completes late
	r.settle()
	r.wantHangups(fc, 1, 0)
}

// A CLIP never ends a call.
func TestCLIPAfterSetupClearedAndCallActiveContinues(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "")
	r.callActive()
	r.tel.send(hfp.CallerID, "+380501111111")
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatalf("state %s", r.c.Status().State)
	}
	a, _ := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
	r.wantHangups(fc, 0, 0)
}
