package bridge

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// ---- ownership needs a successful (or ambiguous) ATA ----

// The owner picked up on the handset while our ATA was still queued; the ATA
// then failed. The CallActive was the owner's, so the call is never touched.
func TestATAFailureAfterCallActiveIsNotOurs(t *testing.T) {
	for _, errv := range []error{hfp.ErrNotSent, hfp.ErrResponse, hfp.ErrClosed} {
		for _, bridged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/bridged=%v", errv, bridged), func(t *testing.T) {
				r := newRig(t, noMediaTimeout)
				r.tel.gate = make(chan struct{})
				r.tel.setAnswerErr(errv)
				r.start()
				fc := r.answer()
				if bridged {
					r.connect()
				} else {
					r.callActive()
				}
				close(r.tel.gate)
				e := r.waitEnded()
				if e.Outcome != OutcomeOwnerAnswered || e.HangupBy != "" {
					t.Errorf("ended = %+v", e)
				}
				r.wantHangups(fc, 1, 0)
			})
		}
	}
}

// Ownership also fails closed while the ATA result is unknown.
func TestNoCHUPWhileATAPending(t *testing.T) {
	r := newRig(t, noMediaTimeout)
	r.tel.gate = make(chan struct{})
	r.start()
	fc := r.answer()
	r.connect()
	r.cancel()
	<-r.done
	r.done <- nil
	if _, h := r.tel.counts(); h != 0 || fc.hangups() != 1 {
		t.Errorf("BYE=%d AT+CHUP=%d", fc.hangups(), h)
	}
}

// ---- stale AT commands ----

func TestUnsentATADiscardedWhenCallEnds(t *testing.T) {
	r := newRig(t, nil)
	r.tel.gate = make(chan struct{}) // ATA stuck behind a slow command queue
	r.start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	// The cancelled ATA reports "not sent": the phone was never answered, so a
	// CallActive later is not ours.
	r.settle()
	r.tel.send(hfp.CallActive, "")
	r.wantHangups(fc, 0, 0)
}

func TestCHUPCancelledByCallEndedOrRing(t *testing.T) {
	for _, ev := range []hfp.EventKind{hfp.CallEnded, hfp.CallSetupCleared, hfp.Ring} {
		t.Run(ev.String(), func(t *testing.T) {
			r := newRig(t, noMediaTimeout)
			r.tel.hangupGate = make(chan struct{})
			r.start()
			fc, _ := r.bridge()
			fc.ended <- sip.TransportLost // AT+CHUP goes out, held by the slow phone
			r.waitEnded()
			r.settle()
			r.tel.send(ev, "")
			r.settle()
			close(r.tel.hangupGate) // the queue drains: the stale CHUP must be gone
			r.settle()
			if _, h := r.tel.counts(); h != 0 {
				t.Errorf("stale AT+CHUP reached the phone (%d)", h)
			}
		})
	}
}

// ---- reversed setup indicators ----

func TestSetupClearedBeforeCallActiveProceeds(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "") // callsetup=0 before call=1
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("ended on the early callsetup=0")
	}
	r.connect()
	r.wantHangups(fc, 0, 0)
}

func TestSetupClearedWithoutCallActiveAbandons(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "")
	r.waitFor("CallActive window", func() bool { return r.clock.hasTimer(5 * time.Second) })
	r.clock.Advance(4999 * time.Millisecond)
	r.settle()
	if r.c.Status().State != "answering" {
		t.Fatal("ended inside the window")
	}
	r.clock.Advance(time.Millisecond)
	e := r.waitEnded()
	if e.Outcome != OutcomeCallerAbandoned || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

func TestSetupClearedThenATAFailure(t *testing.T) {
	r := newRig(t, nil)
	r.tel.gate = make(chan struct{})
	r.tel.setAnswerErr(hfp.ErrResponse)
	r.start()
	fc := r.answer()
	r.tel.send(hfp.CallSetupCleared, "")
	r.settle()
	close(r.tel.gate)
	e := r.waitEnded()
	if e.Outcome != OutcomeCallerAbandoned || e.HangupBy != HangupCaller {
		t.Errorf("ended = %+v", e)
	}
	r.wantHangups(fc, 1, 0)
}

// ---- the SCO-loss latch ----

func TestSCOLossLatchesNoCHUP(t *testing.T) {
	for name, trigger := range map[string]func(r *rig){
		"media error":  func(r *rig) { r.sip.media(0).errCh <- errBoom },
		"max duration": func(r *rig) { r.clock.Advance(time.Second) },
		"shutdown":     func(r *rig) { r.cancel(); <-r.done; r.done <- nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, func(c *config.Config) { c.Calls.MediaTimeoutSeconds = 0; c.Calls.MaxDurationSeconds = 1 }).start()
			fc, phone := r.bridge()
			phone.Close()
			r.waitFor("SCO lost", func() bool { return strings.Contains(r.log.String(), "SCO lost") })
			trigger(r)
			r.waitFor("ended", func() bool { return len(r.hooks.named("call.ended")) == 1 })
			r.wantHangups(fc, 1, 0) // inside the grace the cellular call may be on the handset
		})
	}
}

// ---- late SCO link at shutdown ----

func TestLateSCOLinkClosedAtShutdown(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := newRig(t, nil)
		a, _ := sco.NewPipe(60)
		r.lis.lateLink = a
		r.start()
		r.answer()
		r.callActive()
		r.cancel()
		<-r.done
		r.done <- nil
		if _, err := a.Write(make([]byte, 60)); err == nil {
			t.Fatalf("run %d: the late SCO link was left open", i)
		}
	}
}

func TestLateCallActiveAfterMediaTimeout(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.Calls.MediaTimeoutSeconds = 3 }).start()
	fc := r.answer()
	for i := 0; i < 3; i++ {
		r.clock.Advance(time.Second)
		r.settle()
	}
	e := r.waitEnded()
	if e.Outcome != OutcomeMediaTimeout {
		t.Fatalf("ended = %+v", e)
	}
	r.tel.send(hfp.CallActive, "") // our ATA answered after all
	r.wantHangups(fc, 1, 1)
}

func TestAttachClearsLateWindow(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.ended <- sip.RemoteBye
	r.waitEnded()
	tel2 := newFakeTel()
	r.c.Attach(tel2, r.lis, "AA:BB:CC:DD:EE:FF")
	r.settle()
	tel2.send(hfp.CallActive, "")
	r.settle()
	if _, h := tel2.counts(); h != 0 {
		t.Error("late window survived a new HFP client")
	}
}

func TestCallEndedEmittedEvenIfPayloadPanics(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	fc.setPanicRemote()
	r.tel.send(hfp.CallEnded, "")
	r.waitFor("call.ended", func() bool { return len(r.hooks.named("call.ended")) == 1 })
	e := r.hooks.named("call.ended")[0]
	if e.ID != "call-1" || e.Outcome != OutcomeCallerAbandoned {
		t.Errorf("ended = %+v", e)
	}
	r.waitState("idle")
}

func TestRemoteMediaPrefersLatched(t *testing.T) {
	r := newRig(t, nil).start()
	fc := r.answer()
	lat := netip.MustParseAddrPort("198.51.100.77:6000")
	r.sip.media(0).mu.Lock()
	r.sip.media(0).stats.Latched = lat
	r.sip.media(0).mu.Unlock()
	_ = fc
	r.connect()
	if got := r.hooks.named("call.connected")[0].RemoteMedia; got != lat.String() {
		t.Errorf("remote_media = %q, want %q", got, lat)
	}
}

func TestPumpWarningCarriesCallID(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	_, phone := r.bridge()
	m := r.sip.media(0)
	m.mu.Lock()
	m.writeErr = errors.New("send: no buffer space")
	m.mu.Unlock()
	feed(t, phone)
	r.waitFor("pump warning", func() bool { return strings.Contains(r.log.String(), "rtp send failing") })
	for _, l := range strings.Split(r.log.String(), "\n") {
		if strings.Contains(l, "rtp send failing") && !strings.Contains(l, "call_id=call-1") {
			t.Errorf("no call_id: %s", l)
		}
	}
}

func TestDrainPlaysSubTargetRemnant(t *testing.T) {
	r := newRig(t, noMediaTimeout).start()
	fc := r.answer()
	a, phone := sco.NewPipe(60)
	r.callActive()
	r.lis.links <- a
	r.waitState("bridged")
	var mu sync.Mutex
	nonzero := false
	go func() {
		buf := make([]byte, 60)
		for {
			n, err := phone.Read(buf)
			if err != nil {
				return
			}
			for _, b := range buf[:n] {
				if b != 0 {
					mu.Lock()
					nonzero = true
					mu.Unlock()
				}
			}
		}
	}()
	t.Cleanup(func() { phone.Close() })
	r.clock.Advance(900 * time.Millisecond) // settle over
	r.settle()
	pcm := make([]int16, 320) // 40 ms, below the 60 ms jitter target
	for i := range pcm {
		pcm[i] = 1000
	}
	r.sip.media(0).pl.Push(0, 1, pcm)
	t0 := r.clock.Now()
	fc.ended <- sip.RemoteBye
	for i := 0; i < 60 && len(r.hooks.named("call.ended")) == 0; i++ {
		r.clock.Advance(10 * time.Millisecond)
		time.Sleep(2 * time.Millisecond)
	}
	r.waitEnded()
	if took := r.clock.Now().Sub(t0); took > 300*time.Millisecond {
		t.Errorf("drain took %v", took)
	}
	mu.Lock()
	defer mu.Unlock()
	if !nonzero {
		t.Error("the remnant never reached SCO")
	}
}
