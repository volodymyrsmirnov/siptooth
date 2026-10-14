package bridge

import (
	"fmt"
	"strings"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// Bridged timings (spec 4.5).
const (
	scoLossGrace = 1500 * time.Millisecond // CallEnded may follow a lost SCO link
	drainMax     = 3 * time.Second         // play out the downlink after a far-end BYE
	drainPoll    = 50 * time.Millisecond
)

const (
	timerMaxDuration = "max_duration"
	timerSCOLost     = "sco_lost"
	timerDrainPoll   = "drain_poll"
	timerDrainMax    = "drain_max"
)

// startBridged runs the media pump (spec 6.2) and arms the max-duration timer.
// The pump goroutine waits for the silence ticker to stop first, so the two
// never send RTP at the same time.
func (c *Coordinator) startBridged(call *activeCall) {
	media, link, gen := call.prepared.Media, call.link, call.gen
	settle := time.Duration(c.d.Config.Calls.SCOSettleMs) * time.Millisecond
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer c.recoverGo(gen)
		<-call.silenceDone
		end := RunPump(call.ctx, link, media, settle, c.d.Clock, c.d.Logger.With("call_id", call.id))
		c.post(msg{kind: mPumpEnd, gen: gen, pumpEnd: end})
	}()
	if d := time.Duration(c.d.Config.Calls.MaxDurationSeconds) * time.Second; d > 0 {
		c.startTimer(call, timerMaxDuration, d)
	}
}

func (c *Coordinator) onTelBridged(call *activeCall, ev hfp.Event) {
	if ev.Kind != hfp.CallEnded {
		return
	}
	call.cellularEnded = true
	if call.draining {
		// The far end hung up first; the phone call is over as well.
		c.finish(call, endSpec{outcome: OutcomeCompleted, hangupBy: HangupFarEnd, farEndGone: true})
		return
	}
	c.finish(call, endSpec{outcome: OutcomeCompleted, hangupBy: HangupCaller})
}

func (c *Coordinator) sipEndedBridged(call *activeCall, cause sip.EndCause) {
	if call.draining {
		return
	}
	if call.scoLost {
		// Waiting for CallEnded after a lost SCO link: the phone call is not
		// touched; the grace timer decides, without a BYE.
		call.sipGone = true
		return
	}
	if cause == sip.RemoteBye {
		c.startDrain(call)
		return
	}
	c.finish(call, endSpec{outcome: OutcomeSignalingLost, hangupBy: HangupSiptooth, farEndGone: true,
		hangupPhone: true, errText: "sip " + string(cause)})
}

// startDrain keeps the pump running so buffered downlink audio is heard, then
// ends with AT+CHUP once the playout is empty or after 3 s (spec 4.5).
func (c *Coordinator) startDrain(call *activeCall) {
	call.draining, call.sipGone = true, true
	c.d.Logger.Info("far end hung up, draining downlink", "call_id", call.id)
	// Play out what is left, however little: priming would withhold a remnant
	// below the jitter target and the caller would lose the last fragment.
	if pl := call.prepared.Media.Playout(); pl != nil {
		pl.Flush()
	}
	c.startTimer(call, timerDrainMax, drainMax)
	c.drainCheck(call)
}

func (c *Coordinator) drainCheck(call *activeCall) {
	if pl := call.prepared.Media.Playout(); pl != nil && pl.Depth() > 0 {
		c.startTimer(call, timerDrainPoll, drainPoll)
		return
	}
	c.finishDrain(call)
}

func (c *Coordinator) finishDrain(call *activeCall) {
	c.finish(call, endSpec{outcome: OutcomeCompleted, hangupBy: HangupFarEnd, farEndGone: true, hangupPhone: true})
}

func (c *Coordinator) onBridgedTimer(call *activeCall, name string) {
	if call.state != stateBridged {
		return
	}
	switch name {
	case timerMaxDuration:
		if call.draining {
			return
		}
		c.finish(call, endSpec{outcome: OutcomeMaxDuration, hangupBy: HangupSiptooth, hangupPhone: true})
	case timerSCOLost:
		// No CallEnded within the grace: the owner may have moved the audio to
		// the handset, so the phone call is left alone.
		c.finish(call, endSpec{outcome: OutcomeAudioLost, hangupBy: HangupSiptooth, errText: "SCO link lost"})
	case timerDrainPoll:
		if call.draining {
			c.drainCheck(call)
		}
	case timerDrainMax:
		if call.draining {
			c.finishDrain(call)
		}
	}
}

func (c *Coordinator) onPumpEnd(m msg) {
	call := c.current(m.gen)
	if call == nil || call.state != stateBridged {
		c.discard(m, "call over")
		return
	}
	if strings.HasPrefix(string(m.pumpEnd), pumpPrefix) {
		c.d.Logger.Error("panic in the media pump", "call_id", call.id, "panic", string(m.pumpEnd))
		c.finish(call, endSpec{outcome: OutcomeMediaError, hangupBy: HangupSiptooth, hangupPhone: true, errText: "pump " + string(m.pumpEnd)})
		return
	}
	switch m.pumpEnd {
	case PumpCtx:
	case PumpRTPError:
		c.finish(call, endSpec{outcome: OutcomeMediaError, hangupBy: HangupSiptooth, hangupPhone: true, errText: "rtp send failed"})
	case PumpSCOEOF, PumpSCOStall:
		// From here on the call may be on the handset: no ending of this call
		// sends AT+CHUP any more, not even a timeout inside the grace period.
		call.noCHUP = true
		if call.draining {
			// Nothing left to play to; the far end is gone either way.
			c.finishDrain(call)
			return
		}
		if call.scoLost {
			return
		}
		call.scoLost = true
		c.d.Logger.Warn("SCO lost", "call_id", call.id, "why", string(m.pumpEnd))
		c.startTimer(call, timerSCOLost, scoLossGrace)
	}
}

// ---- panics ----

// onPanic tears down the call in progress after a handler panicked; the loop
// keeps running.
func (c *Coordinator) onPanic(what string, stack []byte) {
	c.d.Logger.Error("panic in coordinator", "panic", what, "stack", string(stack))
	call := c.cur
	if call == nil {
		return
	}
	c.panicTeardown(call, what)
}

func (c *Coordinator) onGoroutinePanic(m msg) {
	call := c.current(m.gen)
	if call == nil {
		c.d.Logger.Error("panic in a call goroutine after the call ended", "error", errString(m.err))
		return
	}
	c.d.Logger.Error("panic in a call goroutine", "call_id", call.id, "error", errString(m.err))
	c.panicTeardown(call, errString(m.err))
}

// panicTeardown ends call per the ownership rule: BYE if the SIP leg is up,
// AT+CHUP only for a phone call siptooth answered.
func (c *Coordinator) panicTeardown(call *activeCall, what string) {
	if call.state != stateEnding {
		outcome := OutcomeMediaError
		if call.state == stateDialing {
			outcome = OutcomeSIPError
		}
		c.finish(call, endSpec{outcome: outcome, hangupBy: HangupSiptooth, hangupPhone: call.ours(),
			errText: fmt.Sprintf("internal error: %s", what)})
	}
	if c.cur == call { // the panic hit finish itself
		c.cur = nil
	}
}
