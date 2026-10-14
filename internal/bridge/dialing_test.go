package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

func (r *rig) assertNoPhoneAction() {
	r.t.Helper()
	if a, h := r.tel.counts(); a != 0 || h != 0 {
		r.t.Errorf("phone touched: answers=%d hangups=%d", a, h)
	}
}

func (r *rig) assertSingleEnded() {
	r.t.Helper()
	r.settle()
	if n := len(r.hooks.named("call.ended")); n != 1 {
		r.t.Errorf("call.ended emitted %d times, want 1 (events %v)", n, r.hooks.names())
	}
}

func TestRingStartsDial(t *testing.T) {
	r := newRig(t, nil).start()
	r.tel.send(hfp.Ring, "")
	r.waitArmed(1) // caller ID wait
	if r.sip.starts() != 0 {
		t.Fatal("INVITE sent before the caller ID arrived")
	}
	r.clock.Advance(200 * time.Millisecond)
	r.tel.events <- hfp.Event{Kind: hfp.CallerID, Number: "380501234567", NumberType: 145}
	r.waitFor("INVITE", func() bool { return r.sip.starts() == 1 })

	req := r.sip.req(0)
	if req.From != "sip:+380501234567@sip.example.com" {
		t.Errorf("From = %q", req.From)
	}
	if req.DisplayName != `"+380501234567"` {
		t.Errorf("DisplayName = %q", req.DisplayName)
	}
	if req.Target != "sip:+380441234567@sip.example.com;transport=tls" {
		t.Errorf("Target = %q", req.Target)
	}
	if req.Headers["X-CALL-ID"] != "call-1" {
		t.Errorf("headers = %v", req.Headers)
	}
	if req.Contact != "sip:siptooth@192.0.2.10:5061;transport=tls" || req.Transport != "tls" || !req.Offer.Addr.IsValid() {
		t.Errorf("request = %+v", req)
	}
	if r.sip.hosts[0] != "sip.example.com" {
		t.Errorf("Prepare host = %q", r.sip.hosts[0])
	}
	// call.ringing precedes the INVITE and carries the caller.
	ringing := r.hooks.named("call.ringing")
	if len(ringing) != 1 {
		t.Fatalf("ringing hooks: %v", r.hooks.names())
	}
	ri := ringing[0]
	if ri.ID != "call-1" || ri.Caller == nil || *ri.Caller != "+380501234567" || ri.CallerWithheld ||
		ri.Number != "+380441234567" || ri.Target != req.Target {
		t.Errorf("ringing = %+v", ri)
	}
	if st := r.c.Status(); st.State != "dialing" || st.CallID != "call-1" || st.Caller != "+380501234567" || st.CallStartedAt == nil {
		t.Errorf("status = %+v", st)
	}
	r.assertNoPhoneAction()
}

func TestCallerIDWaitExpires(t *testing.T) {
	r := newRig(t, nil).start()
	r.tel.send(hfp.Ring, "")
	r.waitArmed(1)
	wait := time.Duration(r.cfg.Calls.CallerIDWaitMs) * time.Millisecond
	r.clock.Advance(wait - time.Millisecond)
	r.settle()
	if r.sip.starts() != 0 {
		t.Fatal("INVITE before the wait expired")
	}
	r.clock.Advance(time.Millisecond)
	r.waitFor("INVITE", func() bool { return r.sip.starts() == 1 })

	req := r.sip.req(0)
	if req.From != "sip:anonymous@sip.example.com" || req.DisplayName != `"anonymous"` {
		t.Errorf("From %q display %q", req.From, req.DisplayName)
	}
	ri := r.hooks.named("call.ringing")[0]
	if ri.Caller != nil || !ri.CallerWithheld {
		t.Errorf("ringing = %+v, want withheld", ri)
	}

	// A +CLIP after the INVITE went out changes nothing.
	r.tel.send(hfp.CallerID, "+1555")
	r.settle()
	if r.sip.starts() != 1 || r.c.Status().Caller != "" {
		t.Error("late caller ID was applied")
	}
}

func TestCallerIDEmptyIsWithheld(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial(" ") // +CLIP with no usable digits
	if ri := r.hooks.named("call.ringing")[0]; ri.Caller != nil || !ri.CallerWithheld {
		t.Errorf("ringing = %+v", ri)
	}
}

func TestNotReadyIgnoresRing(t *testing.T) {
	r := newRig(t, nil).start()
	r.c.SetAudioRouting(false)
	r.waitFor("not ready", func() bool { return !r.c.Status().Ready })
	r.tel.send(hfp.Ring, "")
	r.settle()
	if got := r.hooks.names(); len(got) != 0 {
		t.Errorf("hooks = %v, want none", got)
	}
	if st := r.c.Status(); st.State != "idle" || st.LastOutcome != "" {
		t.Errorf("status = %+v", st)
	}
	if r.sip.prepares() != 0 || r.sip.starts() != 0 {
		t.Error("dial started")
	}
	if !strings.Contains(r.log.String(), "ring ignored") {
		t.Errorf("no log line for the ignored ring: %s", r.log.String())
	}
	r.assertNoPhoneAction()
}

func TestDialRejectedLeavesPhone(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486, Reason: "Busy Here"})
	e := r.waitEnded()
	if e.Outcome != OutcomeSIPRejected || e.SIPStatus != 486 || e.SIPReason != "Busy Here" || e.HangupBy != "" {
		t.Errorf("ended = %+v", e)
	}
	if e.RingingSeconds < 0 || e.BridgedSeconds != nil || e.Media == nil {
		t.Errorf("ended times/media = %+v", e)
	}
	r.assertNoPhoneAction()
	if r.sip.attempt(0).abandons() != 0 {
		t.Error("finished attempt was abandoned")
	}
	if r.sip.media(0).closeCount() != 1 {
		t.Errorf("media closed %d times", r.sip.media(0).closeCount())
	}
	if st := r.c.Status(); st.State != "idle" || st.LastOutcome != OutcomeSIPRejected {
		t.Errorf("status = %+v", st)
	}
	r.assertSingleEnded()
}

func TestDialFailures(t *testing.T) {
	tests := []struct {
		name    string
		res     sip.DialResult
		outcome string
		status  int
		errHas  string
	}{
		{"error", sip.DialResult{Cause: sip.FailError, Reason: "auth_failed", Status: 401, Err: errBoom}, OutcomeSIPError, 401, "boom"},
		{"error without Err", sip.DialResult{Cause: sip.FailError, Reason: "invite_failed"}, OutcomeSIPError, 0, "invite_failed"},
		{"media", sip.DialResult{Cause: sip.FailMedia, Status: 200, Reason: "OK", Err: errBoom}, OutcomeSIPMediaFailed, 200, "boom"},
		{"unexpected abandoned", sip.DialResult{Cause: sip.FailAbandoned}, OutcomeSIPError, 0, "abandoned"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, nil).start()
			r.ringAndDial("+380501234567")
			r.sip.attempt(0).send(tt.res)
			e := r.waitEnded()
			if e.Outcome != tt.outcome || e.SIPStatus != tt.status || !strings.Contains(e.Error, tt.errHas) {
				t.Errorf("ended = %+v", e)
			}
			r.assertNoPhoneAction()
			r.assertSingleEnded()
		})
	}
}

func TestPrepareAndStartFailures(t *testing.T) {
	t.Run("prepare error", func(t *testing.T) {
		r := newRig(t, nil)
		r.sip.prepareErr = errBoom
		r.start()
		r.tel.send(hfp.Ring, "")
		r.tel.send(hfp.CallerID, "+1555")
		e := r.waitEnded()
		if e.Outcome != OutcomeSIPError || !strings.Contains(e.Error, "prepare: boom") {
			t.Errorf("ended = %+v", e)
		}
		if r.sip.starts() != 0 {
			t.Error("INVITE sent")
		}
		r.assertSingleEnded()
	})
	t.Run("start error", func(t *testing.T) {
		r := newRig(t, nil)
		r.sip.startErr = errBoom
		r.start()
		r.tel.send(hfp.Ring, "")
		r.tel.send(hfp.CallerID, "+1555")
		e := r.waitEnded()
		if e.Outcome != OutcomeSIPError || !strings.Contains(e.Error, "boom") {
			t.Errorf("ended = %+v", e)
		}
		if r.sip.media(0).closeCount() != 1 {
			t.Error("media not closed")
		}
		r.assertSingleEnded()
	})
}

func TestDialTimeoutAbandons(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.waitArmed(2) // caller ID wait, dial timer
	r.clock.Advance(29 * time.Second)
	r.settle()
	if r.c.Status().State != "dialing" {
		t.Fatal("ended before the dial timeout")
	}
	r.clock.Advance(time.Second)
	e := r.waitEnded()
	if e.Outcome != OutcomeSIPTimeout {
		t.Errorf("outcome = %s", e.Outcome)
	}
	if n := r.sip.attempt(0).abandons(); n != 1 {
		t.Errorf("abandons = %d", n)
	}
	if r.sip.media(0).closeCount() != 1 {
		t.Error("media not closed")
	}
	r.assertNoPhoneAction()
	r.assertSingleEnded()
}

func TestCallerAbandonedWhileDialing(t *testing.T) {
	t.Run("during caller id wait", func(t *testing.T) {
		r := newRig(t, nil).start()
		r.tel.send(hfp.Ring, "")
		r.waitArmed(1)
		r.tel.send(hfp.CallSetupCleared, "")
		e := r.waitEnded()
		if e.Outcome != OutcomeCallerAbandoned || r.sip.prepares() != 0 {
			t.Errorf("ended = %+v, prepares %d", e, r.sip.prepares())
		}
		// Every call.ended has its call.ringing.
		if got := r.hooks.names(); len(got) != 2 || got[0] != "call.ringing" {
			t.Errorf("hooks = %v", got)
		}
		r.assertSingleEnded()
	})
	t.Run("during invite", func(t *testing.T) {
		r := newRig(t, nil).start()
		r.ringAndDial("+380501234567")
		r.tel.send(hfp.CallSetupCleared, "")
		e := r.waitEnded()
		if e.Outcome != OutcomeCallerAbandoned || r.sip.attempt(0).abandons() != 1 {
			t.Errorf("ended = %+v abandons %d", e, r.sip.attempt(0).abandons())
		}
		r.assertNoPhoneAction()
		r.assertSingleEnded()
	})
}

func TestOwnerAnswersWhileDialing(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.tel.send(hfp.CallActive, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeOwnerAnswered || e.HangupBy != "" {
		t.Errorf("ended = %+v", e)
	}
	if r.sip.attempt(0).abandons() != 1 {
		t.Error("attempt not abandoned")
	}
	r.assertNoPhoneAction() // never Hangup, never Answer
	r.assertSingleEnded()
}

func TestOwnerAnswersDuringPrepare(t *testing.T) {
	r := newRig(t, nil)
	r.sip.gate = make(chan struct{})
	r.start()
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380501234567")
	r.waitFor("prepare", func() bool { return r.sip.prepares() == 1 })
	r.tel.send(hfp.CallActive, "")
	e := r.waitEnded()
	if e.Outcome != OutcomeOwnerAnswered {
		t.Errorf("outcome = %s", e.Outcome)
	}
	close(r.sip.gate) // Prepare finishes after the call is over
	r.waitFor("media created", func() bool { return r.sip.mediaCount() == 1 })
	r.waitFor("stale media closed", func() bool { return r.sip.media(0).closeCount() == 1 })
	r.settle()
	if r.sip.starts() != 0 {
		t.Error("INVITE sent for a dead call")
	}
	r.assertNoPhoneAction()
	r.assertSingleEnded()
}

func TestDisconnectedWhileDialing(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(r *rig)
	}{
		{"hfp disconnected", func(r *rig) { r.tel.send(hfp.Disconnected, "") }},
		{"detach", func(r *rig) { r.c.Detach() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil).start()
			r.ringAndDial("+380501234567")
			tc.do(r)
			e := r.waitEnded()
			if e.Outcome != OutcomeBluetoothLost {
				t.Errorf("outcome = %s", e.Outcome)
			}
			if r.sip.attempt(0).abandons() != 1 {
				t.Error("attempt not abandoned")
			}
			if st := r.c.Status(); st.PhoneConnected || st.Ready {
				t.Errorf("status = %+v", st)
			}
			r.assertNoPhoneAction()
			r.assertSingleEnded()
		})
	}
}

func TestShutdownWhileDialing(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.cancel()
	<-r.done
	r.done <- nil
	e := r.hooks.named("call.ended")
	if len(e) != 1 || e[0].Outcome != OutcomeShutdown {
		t.Fatalf("ended = %+v", e)
	}
	if r.sip.attempt(0).abandons() != 1 || r.sip.media(0).closeCount() != 1 {
		t.Error("attempt or media not released")
	}
	r.assertNoPhoneAction()
}

func TestShutdownDuringPrepare(t *testing.T) {
	r := newRig(t, nil)
	r.sip.gate = make(chan struct{})
	r.start()
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380501234567")
	r.waitFor("prepare", func() bool { return r.sip.prepares() == 1 })
	r.cancel()
	r.waitFor("call.ended", func() bool { return len(r.hooks.named("call.ended")) == 1 })
	select {
	case <-r.done:
		t.Fatal("Run returned while Prepare still held a socket")
	case <-time.After(30 * time.Millisecond):
	}
	close(r.sip.gate)
	<-r.done
	r.done <- nil
	if r.sip.mediaCount() != 1 || r.sip.media(0).closeCount() != 1 {
		t.Errorf("media leaked: created %d", r.sip.mediaCount())
	}
	if e := r.hooks.named("call.ended")[0]; e.Outcome != OutcomeShutdown {
		t.Errorf("outcome = %s", e.Outcome)
	}
	if r.sip.starts() != 0 {
		t.Error("INVITE sent after shutdown")
	}
}

func TestRingWhileBusyIgnored(t *testing.T) {
	t.Run("dialing", func(t *testing.T) {
		r := newRig(t, nil).start()
		r.ringAndDial("+380501234567")
		r.tel.send(hfp.Ring, "") // call waiting / repeated RING
		r.settle()
		if r.sip.starts() != 1 || len(r.hooks.named("call.ringing")) != 1 || r.c.Status().CallID != "call-1" {
			t.Errorf("second ring disturbed the call: hooks %v", r.hooks.names())
		}
	})
	t.Run("answering", func(t *testing.T) {
		r := newRig(t, nil).start()
		r.ringAndDial("+380501234567")
		r.sip.attempt(0).send(sip.DialResult{Call: newFakeCall(), Status: 200, Reason: "OK"})
		r.waitState("answering")
		r.tel.send(hfp.Ring, "")
		r.settle()
		if r.sip.starts() != 1 || len(r.hooks.named("call.ringing")) != 1 || r.c.Status().State != "answering" {
			t.Errorf("second ring disturbed the call: hooks %v", r.hooks.names())
		}
	})
}

func TestLateResultDiscarded(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.waitArmed(2)
	r.clock.Advance(30 * time.Second)
	r.waitEnded()

	// The attempt reports an answer after the call is over: not adopted, the
	// dialog is hung up, nothing else happens.
	fc := newFakeCall()
	r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
	r.waitFor("late dialog hung up", func() bool { return fc.hangups() == 1 })
	r.settle()
	if st := r.c.Status(); st.State != "idle" || st.LastOutcome != OutcomeSIPTimeout {
		t.Errorf("status = %+v", st)
	}
	r.assertNoPhoneAction()
	r.assertSingleEnded()
}

func TestLateResultOfOldGenerationIgnoredByNewCall(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.tel.send(hfp.CallSetupCleared, "")
	r.waitEnded()

	// A new call starts; the old attempt's late failure must not end it.
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380509999999")
	r.waitFor("second INVITE", func() bool { return r.sip.starts() == 2 })
	r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486, Reason: "Busy Here"})
	r.settle()
	if st := r.c.Status(); st.State != "dialing" || st.CallID != "call-2" {
		t.Errorf("status = %+v", st)
	}
	if len(r.hooks.named("call.ended")) != 1 {
		t.Errorf("hooks = %v", r.hooks.names())
	}
}

func TestAnsweredEntersAnswering(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Calls.MediaTimeoutSeconds = 0 }).start()
	r.ringAndDial("+380501234567")
	fc := newFakeCall()
	r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
	r.waitState("answering")
	r.waitFor("ATA", func() bool { a, _ := r.tel.counts(); return a == 1 })
	r.tel.send(hfp.CallActive, "")
	a, phone := sco.NewPipe(60)
	r.lis.links <- a
	r.waitState("bridged")
	feed(t, phone)
	r.clock.Advance(time.Minute) // the dial timer no longer applies
	r.settle()
	if r.c.Status().State != "bridged" {
		t.Fatalf("state = %s: the dial timer ended an answered call", r.c.Status().State)
	}
}

// Every way a call can end in Dialing and Answering emits exactly one
// call.ended, even when more events for the dead call keep arriving.
func TestExactlyOneCallEnded(t *testing.T) {
	answered := func(r *rig) *fakeCall {
		fc := newFakeCall()
		r.sip.attempt(0).send(sip.DialResult{Call: fc, Status: 200, Reason: "OK"})
		r.waitState("answering")
		return fc
	}
	scenarios := []struct {
		name     string
		outcome  string
		shutdown bool
		do       func(r *rig)
	}{
		{"rejected", OutcomeSIPRejected, false, func(r *rig) {
			r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 603, Reason: "Decline"})
		}},
		{"sip error", OutcomeSIPError, false, func(r *rig) {
			r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailError, Err: errBoom})
		}},
		{"media failed", OutcomeSIPMediaFailed, false, func(r *rig) {
			r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailMedia, Err: errBoom})
		}},
		{"timeout", OutcomeSIPTimeout, false, func(r *rig) { r.waitArmed(2); r.clock.Advance(30 * time.Second) }},
		{"caller abandoned", OutcomeCallerAbandoned, false, func(r *rig) { r.tel.send(hfp.CallSetupCleared, "") }},
		{"owner answered", OutcomeOwnerAnswered, false, func(r *rig) { r.tel.send(hfp.CallActive, "") }},
		{"disconnected", OutcomeBluetoothLost, false, func(r *rig) { r.tel.send(hfp.Disconnected, "") }},
		{"shutdown", OutcomeShutdown, true, func(r *rig) { r.cancel() }},
		{"answered then far end", OutcomeFarEndEnded, false, func(r *rig) { answered(r).ended <- sip.RemoteBye }},
		{"answered then caller", OutcomeCallerAbandoned, false, func(r *rig) { answered(r); r.tel.send(hfp.CallEnded, "") }},
		{"answered then shutdown", OutcomeShutdown, true, func(r *rig) { answered(r); r.cancel() }},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			r := newRig(t, nil).start()
			r.ringAndDial("+380501234567")
			s.do(r)
			if s.shutdown {
				<-r.done
				r.done <- nil
			} else {
				r.waitEnded()
			}
			// Everything that can still arrive for the dead call.
			r.sip.attempt(0).send(sip.DialResult{Call: newFakeCall(), Status: 200})
			if !s.shutdown {
				for _, k := range []hfp.EventKind{hfp.CallActive, hfp.CallSetupCleared, hfp.CallEnded, hfp.CallerID} {
					r.tel.send(k, "+1")
				}
				r.clock.Advance(2 * time.Minute)
			}
			r.settle()
			ended := r.hooks.named("call.ended")
			if len(ended) != 1 || ended[0].Outcome != s.outcome {
				t.Errorf("call.ended = %+v, want one %s", ended, s.outcome)
			}
			if ringing := r.hooks.named("call.ringing"); len(ringing) != 1 {
				t.Errorf("call.ringing x%d", len(ringing))
			}
			if !s.shutdown && r.c.Status().State != "idle" {
				t.Errorf("state = %s", r.c.Status().State)
			}
		})
	}
}

func TestNewCallStartsAfterEnded(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486})
	r.waitEnded()
	r.tel.send(hfp.Ring, "")
	r.tel.send(hfp.CallerID, "+380509999999")
	r.waitFor("second INVITE", func() bool { return r.sip.starts() == 2 })
	if r.sip.req(1).Headers["X-CALL-ID"] != "call-2" {
		t.Errorf("headers = %v", r.sip.req(1).Headers)
	}
}

func TestStatusAndRegistration(t *testing.T) {
	r := newRig(t, nil)
	if st := r.c.Status(); st.State != "idle" || st.Ready || st.PhoneConnected {
		t.Fatalf("initial status = %+v", st)
	}
	r.start()
	exp := time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC)
	r.c.SetRegistration(RegistrationStatus{Enabled: true, Registered: true, Contact: "sip:me@192.0.2.1", Expires: &exp})
	r.waitFor("registration", func() bool { return r.c.Status().Registration.Registered })
	st := r.c.Status()
	if !st.Ready || !st.PhoneConnected || st.Registration.Contact != "sip:me@192.0.2.1" {
		t.Errorf("status = %+v", st)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ready":true`, `"phone_connected":true`, `"state":"idle"`, `"registration":{"enabled":true,"registered":true`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("status JSON %s lacks %s", b, key)
		}
	}
}

func TestSummaryLogLine(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486, Reason: "Busy Here"})
	r.waitEnded()
	var line string
	for _, l := range strings.Split(r.log.String(), "\n") {
		if strings.Contains(l, `msg="call ended"`) {
			line = l
		}
	}
	for _, want := range []string{"call_id=call-1", "caller=+380501234567", "outcome=sip_rejected", "hangup_by=", "sip_status=486",
		"ringing_s=", "bridged_s=", "codec=", "nat_mode=local", "lost=0", "late=0", "discarded_silence=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary line lacks %q: %s", want, line)
		}
	}
	if n := strings.Count(r.log.String(), `msg="call ended"`); n != 1 {
		t.Errorf("%d summary lines", n)
	}
}

// Ending with hangupPhone runs AT+CHUP in the background; Answering and
// Bridged (later tasks) use it. Loop-owned state is poked directly, so Run is
// not started.
func TestFinishHangsUpPhoneOnceInBackground(t *testing.T) {
	r := newRig(t, nil)
	r.c.tel = r.tel
	call := &activeCall{id: "call-x", gen: 1, state: stateAnswering, callActive: true, ata: ataOK, timerGen: map[string]int{}, ringAt: r.clock.Now()}
	call.ctx, call.cancel = t.Context(), func() {}
	r.c.cur = call
	e := endSpec{outcome: OutcomeFarEndEnded, hangupBy: HangupFarEnd, hangupPhone: true}
	r.c.finish(call, e)
	r.c.finish(call, e)
	r.c.bg.Wait()
	if _, h := r.tel.counts(); h != 1 {
		t.Errorf("hangups = %d, want 1", h)
	}
	if n := len(r.hooks.named("call.ended")); n != 1 {
		t.Errorf("call.ended x%d", n)
	}
	if r.c.cur != nil || r.c.lastOutcome != OutcomeFarEndEnded {
		t.Error("did not return to idle")
	}
}

func TestNextHopHost(t *testing.T) {
	tests := []struct{ proxy, target, want string }{
		{"", "sip:+380441234567@sip.example.com;transport=tls", "sip.example.com"},
		{"", "sip:600@192.168.1.10:5070", "192.168.1.10"},
		{"", "sip:sip.example.com", "sip.example.com"},
		{"", "sip:a@[2001:db8::1]:5060;transport=udp", "2001:db8::1"},
		{"", "sip:a@host?h=x@y", "host"},
		{"proxy.example.net:5061", "sip:600@sip.example.com", "proxy.example.net"},
		{"proxy.example.net", "sip:600@sip.example.com", "proxy.example.net"},
	}
	for _, tt := range tests {
		if got := nextHopHost(tt.proxy, tt.target); got != tt.want {
			t.Errorf("nextHopHost(%q, %q) = %q, want %q", tt.proxy, tt.target, got, tt.want)
		}
	}
}

func TestProxyIsNextHop(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.SIP.Proxy = "proxy.example.net:5061" }).start()
	r.ringAndDial("+380501234567")
	if r.sip.hosts[0] != "proxy.example.net" {
		t.Errorf("Prepare host = %q", r.sip.hosts[0])
	}
	if r.sip.req(0).NextHop != "proxy.example.net:5061" {
		t.Errorf("NextHop = %q", r.sip.req(0).NextHop)
	}
}

func TestSIPResponseLogged(t *testing.T) {
	r := newRig(t, nil).start()
	r.ringAndDial("+380501234567")
	r.sip.req(0).OnResponse(180, "Ringing")
	out := r.log.String()
	for _, want := range []string{`msg="sip response"`, "call_id=call-1", "status=180", "reason=Ringing"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

// hangup_by is null for endings during Dialing, except when siptooth itself
// cuts the call short (shutdown, lost Bluetooth).
func TestHangupByWhileDialing(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *rig)
		want string
	}{
		{"rejected", func(r *rig) { r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailRejected, Status: 486}) }, ""},
		{"error", func(r *rig) { r.sip.attempt(0).send(sip.DialResult{Cause: sip.FailError, Err: errBoom}) }, ""},
		{"timeout", func(r *rig) { r.waitArmed(2); r.clock.Advance(30 * time.Second) }, ""},
		{"caller abandoned", func(r *rig) { r.tel.send(hfp.CallSetupCleared, "") }, ""},
		{"owner answered", func(r *rig) { r.tel.send(hfp.CallActive, "") }, ""},
		{"bluetooth lost", func(r *rig) { r.tel.send(hfp.Disconnected, "") }, HangupSiptooth},
		{"shutdown", func(r *rig) { r.cancel() }, HangupSiptooth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, nil).start()
			r.ringAndDial("+380501234567")
			tt.do(r)
			var e hooks.CallInfo
			if tt.name == "shutdown" {
				<-r.done
				r.done <- nil
				e = r.hooks.named("call.ended")[0]
			} else {
				e = r.waitEnded()
			}
			if e.HangupBy != tt.want {
				t.Errorf("hangup_by = %q, want %q", e.HangupBy, tt.want)
			}
		})
	}
}
