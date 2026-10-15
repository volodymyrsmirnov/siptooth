package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// ---- the owner's outgoing call is never ours ----

// The late window is armed (ATA ok, SIP ended before CallActive); the owner dials out. The
// CallActive that follows is the owner's call, not the old ATA's.
func TestOutgoingSetupCancelsLateWindow(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	r.tel.send(hfp.OutgoingSetup, "")
	r.tel.send(hfp.CallActive, "")
	r.settle()
	r.wantHangups(fc, 0, 0)
}

func TestOutgoingSetupCancelsPendingCHUP(t *testing.T) {
	r := newRig(t, nil)
	r.tel.hangupGate = make(chan struct{})
	r.start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	r.tel.send(hfp.CallActive, "") // proves the ATA answered: AT+CHUP queued behind the gate
	r.waitFor("CHUP in flight", func() bool { return r.c.Status().CleanupPending })
	r.tel.send(hfp.OutgoingSetup, "")
	r.settle()
	close(r.tel.hangupGate)
	r.wantHangups(fc, 0, 0)
}

func TestOutgoingSetupWhileDialingIsOwnerActivity(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.tel.send(hfp.OutgoingSetup, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeOwnerAnswered {
		t.Fatalf("ended = %+v", e)
	}
	r.tel.send(hfp.CallActive, "")
	r.settle()
	if a, h := r.tel.counts(); a != 0 || h != 0 {
		t.Fatalf("ATA %d CHUP %d, want none", a, h)
	}
}

func TestOutgoingSetupWhileAnsweringNeverHangsUpThePhone(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.tel.send(hfp.OutgoingSetup, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeOwnerAnswered {
		t.Fatalf("ended = %+v", e)
	}
	r.tel.send(hfp.CallActive, "")
	r.settle()
	r.wantHangups(fc, 1, 0)
}

// ---- callsetup=0 before call=1 must not close the late window ----

func TestLateWindowSurvivesSetupClearedWhenATAWorked(t *testing.T) {
	for name, err := range map[string]error{"ok": nil, "ambiguous": hfp.ErrTimeout} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.tel.setAnswerErr(err)
			r.start()
			fc := r.answerAny()
			r.waitFor("window", func() bool { return r.clock.hasTimer(5*time.Second) || r.c.Status().State != "answering" })
			fc.ended <- sip.RemoteBye
			r.waitEnded()
			r.tel.send(hfp.CallSetupCleared, "")
			r.settle()
			r.tel.send(hfp.CallActive, "")
			r.wantHangups(fc, 0, 1)
		})
	}
}

func TestLateWindowEndsOnNewRing(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	r.tel.send(hfp.Ring, "") // a new call: a later CallActive is its owner's, not the old ATA's
	r.tel.send(hfp.CallActive, "")
	r.settle()
	r.wantHangups(fc, 0, 0)
}

// ---- shutdown resolves a pending ATA ----

func TestShutdownWaitsForPendingATAWithCallActive(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		chups int
	}{"ok": {nil, 1}, "ambiguous": {hfp.ErrTimeout, 1}, "rejected": {hfp.ErrResponse, 0}, "not sent": {hfp.ErrNotSent, 0}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.tel.gate, r.tel.ignoreCtx = make(chan struct{}), true
			release := sync.OnceFunc(func() { close(r.tel.gate) })
			t.Cleanup(release)
			r.tel.setAnswerErr(tc.err)
			r.start()
			fc := r.answer()
			r.callActive()
			r.cancel()
			r.settle()
			select {
			case <-r.done:
				t.Fatal("shutdown did not wait for the pending ATA")
			default:
			}
			release()
			<-r.done
			r.done <- nil
			if _, h := r.tel.counts(); h != tc.chups || fc.hangups() != 1 {
				t.Errorf("CHUP=%d BYE=%d, want %d and 1", h, fc.hangups(), tc.chups)
			}
		})
	}
}

// The wait is bounded: an ATA that never answers does not hold the shutdown's decision.
func TestShutdownATAWaitIsBounded(t *testing.T) {
	r := newRig(t, nil)
	r.tel.gate, r.tel.ignoreCtx = make(chan struct{}), true // the command is on the wire and never answered
	release := sync.OnceFunc(func() { close(r.tel.gate) })
	t.Cleanup(release)
	r.start()
	r.answer()
	r.callActive()
	r.cancel()
	r.waitFor("shutdown wait timer", func() bool { return r.clock.hasTimer(3 * time.Second) })
	r.clock.Advance(3 * time.Second)
	r.waitFor("bound reached", func() bool { return strings.Contains(r.log.String(), "ATA result still unknown") })
	release() // only the goroutine join remains
	select {
	case <-r.done:
		r.done <- nil
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if _, h := r.tel.counts(); h != 0 {
		t.Errorf("CHUP=%d without ownership", h)
	}
}

// ---- the phone's physical state ----

func TestPhoneBusyAfterSIPFailureUntilSetupClears(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486, Reason: "Busy Here"})
	r.waitEnded()
	if st := r.c.Status(); st.State != "idle" || !st.PhoneBusy || st.CleanupPending {
		t.Fatalf("status = %+v: the phone still rings", st)
	}
	r.tel.send(hfp.CallSetupCleared, "")
	r.waitFor("phone free", func() bool { return !r.c.Status().PhoneBusy })
}

func TestPhoneBusyWhileOwnerHasACall(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.tel.send(hfp.CallActive, "")
	r.waitEnded()
	if !r.c.Status().PhoneBusy {
		t.Fatal("owner answered: the phone carries a call")
	}
	r.tel.send(hfp.CallEnded, "")
	r.waitFor("phone free", func() bool { return !r.c.Status().PhoneBusy })
	r.tel.send(hfp.OutgoingSetup, "")
	r.waitFor("outgoing busy", func() bool { return r.c.Status().PhoneBusy })
	r.tel.send(hfp.CallSetupCleared, "")
	r.waitFor("outgoing aborted", func() bool { return !r.c.Status().PhoneBusy })
}

func TestCleanupPendingDuringLateWindowAndCHUP(t *testing.T) {
	r := newRig(t, nil)
	r.tel.hangupGate = make(chan struct{})
	r.start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	if !r.c.Status().CleanupPending {
		t.Fatal("late window armed: cleanup pending")
	}
	r.tel.send(hfp.CallActive, "")
	r.waitFor("CHUP in flight", func() bool { _, ok := r.chupInFlight(); return ok })
	if !r.c.Status().CleanupPending {
		t.Fatal("AT+CHUP in flight: cleanup pending")
	}
	close(r.tel.hangupGate)
	r.waitFor("cleanup done", func() bool { return !r.c.Status().CleanupPending })
}

func (r *rig) chupInFlight() (int, bool) {
	r.c.chupMu.Lock()
	defer r.c.chupMu.Unlock()
	return len(r.c.chups), len(r.c.chups) > 0
}

func TestCleanupPendingDuringBackgroundBYE(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.mu.Lock()
	fc.hangupGate = make(chan struct{})
	fc.mu.Unlock()
	r.tel.send(hfp.CallEnded, "") // caller abandoned; BYE goes out
	r.waitFor("BYE in flight", func() bool { return r.c.Status().State == "idle" })
	if !r.c.Status().CleanupPending {
		t.Fatal("BYE in flight: cleanup pending")
	}
	close(fc.hangupGate)
	r.waitFor("cleanup done", func() bool { return !r.c.Status().CleanupPending })
}
