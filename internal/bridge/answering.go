package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// Answering timings (spec 4.4, allochka callActiveTimeout and SCO setup).
const (
	callActiveTimeout = 5 * time.Second // wait for CallActive after the ATA result
	scoDialAfter      = 2 * time.Second // dial SCO when the phone did not connect by itself
	scoLimit          = 5 * time.Second // from CallActive to SCO up
	silenceEvery      = 20 * time.Millisecond
	lateActiveWindow  = 5 * time.Second // CallActive after the SIP side ended
	mediaPollEvery    = time.Second
)

const (
	timerCallActive = "call_active"
	timerSCODial    = "sco_dial"
	timerSCOLimit   = "sco_limit"
	timerMediaPoll  = "media_poll"
)

// ---- Answering (spec 4.4) ----

// enterAnswering starts the media (silence every 20 ms keeps the NAT mapping
// open and lets the far end latch), then sends ATA asynchronously.
func (c *Coordinator) enterAnswering(call *activeCall, res sip.DialResult) {
	c.stopTimer(call, timerDial)
	call.state = stateAnswering
	call.sipCall = res.Call
	call.sipStat, call.sipRsn = res.Status, res.Reason
	c.d.Logger.Info("sip answered", "call_id", call.id, "sip_call_id", res.Call.SIPCallID(), "status", res.Status)

	sc, gen := res.Call, call.gen
	go func() {
		defer c.recoverGo(gen)
		select {
		case cause := <-sc.Ended():
			c.post(msg{kind: mSIPEnded, gen: gen, cause: cause})
		case <-call.ctx.Done():
		case <-c.done:
		}
	}()
	go func() {
		defer c.recoverGo(gen)
		for {
			select {
			case m := <-sc.MediaUpdates():
				c.post(msg{kind: mMediaUpdate, gen: gen, media: m})
			case <-call.ctx.Done():
				return
			case <-c.done:
				return
			}
		}
	}()

	key := call.prepared.LocalKey
	if len(key) == 0 {
		key = call.prepared.Offer.LocalKey
	}
	media := call.prepared.Media
	if err := media.Start(sc.Remote(), key); err != nil {
		// Nothing was asked of the phone yet.
		c.finish(call, endSpec{outcome: OutcomeMediaError, hangupBy: HangupSiptooth, errText: "media start: " + err.Error()})
		return
	}
	call.mediaStartedAt = c.d.Clock.Now()
	call.silenceStop, call.silenceDone = make(chan struct{}), make(chan struct{})
	go c.runSilence(call, media)
	go func() {
		defer c.recoverGo(gen)
		select {
		case err := <-media.Err():
			c.post(msg{kind: mMediaErr, gen: gen, err: err})
		case <-call.ctx.Done():
		case <-c.done:
		}
	}()
	c.startTimer(call, timerMediaPoll, mediaPollEvery)

	// The call is siptooth's from the moment ATA goes out, but the phone only
	// counts as answered once CallActive is observed (spec 4.1).
	call.answerSent = true
	tel := c.tel
	// The ATA belongs to this call: teardown cancels it, and a command that was
	// still queued then reports ErrNotSent instead of reaching a later call.
	ctx, cancel := context.WithTimeout(call.ctx, 10*time.Second)
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer cancel()
		defer c.recoverGo(gen)
		err := tel.Answer(ctx)
		c.post(msg{kind: mAnswer, gen: gen, err: err})
	}()
}

// runSilence sends 20 ms RTP silence until SCO is up (then the pump takes
// over) or the call ends.
func (c *Coordinator) runSilence(call *activeCall, m Media) {
	defer close(call.silenceDone)
	defer c.recoverGo(call.gen)
	t := c.d.Clock.NewTicker(silenceEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C():
			if err := m.WriteSilence(); err != nil {
				c.d.Logger.Debug("silence send failed", "call_id", call.id, "error", err)
			}
		case <-call.silenceStop:
			return
		case <-call.ctx.Done():
			return
		}
	}
}

// ataTimedOut: the command was sent but no final result arrived in time, so
// the phone may have answered anyway. ErrNotSent wraps ErrTimeout but means
// the phone never saw the command.
func ataTimedOut(err error) bool {
	return errors.Is(err, hfp.ErrTimeout) && !errors.Is(err, hfp.ErrNotSent)
}

// ataDefinite: the phone was certainly not answered by our ATA. A context
// error alone (our own cancellation at teardown) says nothing either way.
func ataDefinite(err error) bool {
	if err == nil || ataTimedOut(err) {
		return false
	}
	if errors.Is(err, hfp.ErrNotSent) {
		return true
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (c *Coordinator) onAnswerResult(m msg) {
	if w := c.late; w != nil && w.gen == m.gen {
		// The call ended before this result. A definite failure means the
		// phone was never answered, so a CallActive is not ours.
		if w.ata == ataPending || w.ata == ataAmbiguous {
			w.ata = ataStateOf(m.err)
		}
		c.resolveLate()
		return
	}
	call := c.current(m.gen)
	if call == nil || (call.state != stateAnswering && call.state != stateBridged) {
		c.discard(m, "call over")
		return
	}
	switch {
	case m.err == nil:
		call.ata = ataOK
	case !ataDefinite(m.err):
		call.ata = ataAmbiguous
		c.d.Logger.Warn("ATA timed out, waiting for CallActive", "call_id", call.id, "error", m.err)
	default:
		call.ata = ataFailed
		switch {
		case call.callActive:
			// The CallActive was the owner answering on the handset while our
			// ATA was still queued: the phone call is never touched.
			c.finish(call, endSpec{outcome: OutcomeOwnerAnswered, errText: "ATA: " + m.err.Error()})
		case call.setupCleared:
			c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: HangupCaller})
		default:
			// Definite failure: the phone is left alone (still ringing, or ending).
			c.finish(call, endSpec{outcome: OutcomeAnswerFailed, hangupBy: HangupSiptooth, errText: "ATA: " + m.err.Error()})
		}
		return
	}
	if call.state == stateAnswering && !call.callActive {
		c.startTimer(call, timerCallActive, callActiveTimeout)
	}
}

func (c *Coordinator) onTelAnswering(call *activeCall, ev hfp.Event) {
	switch ev.Kind {
	case hfp.CallerID:
		if call.setupCleared {
			// +CLIP of a new ring: the old call is over.
			c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: HangupCaller})
		}
	case hfp.CallActive:
		if call.callActive {
			return
		}
		call.callActive = true
		call.ringEnd = c.d.Clock.Now()
		c.stopTimer(call, timerCallActive)
		c.d.Logger.Info("phone call answered", "call_id", call.id)
		c.startSCO(call)
	case hfp.CallSetupCleared:
		if call.callActive {
			return // not emitted while active
		}
		// callsetup=0 can arrive before call=1 after ATA (allochka): the
		// CallActive window decides.
		call.setupCleared = true
	case hfp.CallEnded:
		c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: HangupCaller})
	}
}

func (c *Coordinator) onSIPEnded(m msg) {
	call := c.current(m.gen)
	if call == nil {
		c.discard(m, "call over")
		return
	}
	switch call.state {
	case stateAnswering:
		c.sipEndedAnswering(call, m.cause)
	case stateBridged:
		c.sipEndedBridged(call, m.cause)
	default:
		c.discard(m, "call over")
	}
}

func (c *Coordinator) sipEndedAnswering(call *activeCall, cause sip.EndCause) {
	// Before CallActive the phone is left alone; finish arms the late-CallActive
	// window (spec 4.4). After CallActive it hangs up if the call is ours.
	c.finish(call, endSpec{outcome: OutcomeFarEndEnded, hangupBy: HangupFarEnd, farEndGone: true,
		errText: "sip " + string(cause), hangupPhone: true})
}

// ---- late CallActive window (spec 4.4, last paragraph) ----

// lateWatch is the window after a call ended in which a CallActive may still
// prove that our ATA answered the phone. The phone call is ours only when
// CallActive was seen AND the ATA result is ok or ambiguous; whichever of the
// two is still unknown is waited for, bounded by the window.
type lateWatch struct {
	gen        int
	id         string
	stop       chan struct{}
	callActive bool
	ata        ataState
}

// resolveLate hangs up the phone once the call proves to be ours, and closes
// the window on a definite ATA failure.
func (c *Coordinator) resolveLate() {
	w := c.late
	switch {
	case w == nil:
	case w.ata == ataFailed:
		c.clearLate()
	case w.callActive && (w.ata == ataOK || w.ata == ataAmbiguous):
		c.d.Logger.Warn("our ATA answered the phone after the call ended, hanging up the phone", "call_id", w.id)
		c.clearLate()
		if c.tel != nil {
			c.hangupPhone(c.tel, w.id)
		}
	}
}

func ataStateOf(err error) ataState {
	switch {
	case err == nil:
		return ataOK
	case ataDefinite(err):
		return ataFailed
	}
	return ataAmbiguous
}

func (c *Coordinator) armLate(call *activeCall) {
	c.clearLate()
	w := &lateWatch{gen: call.gen, id: call.id, stop: make(chan struct{}), callActive: call.callActive, ata: call.ata}
	c.late = w
	t := c.d.Clock.NewTimer(lateActiveWindow)
	go func() {
		select {
		case <-t.C():
			c.post(msg{kind: mLate, gen: w.gen})
		case <-w.stop:
			t.Stop()
		case <-c.done:
			t.Stop()
		}
	}()
}

func (c *Coordinator) clearLate() {
	if c.late != nil {
		close(c.late.stop)
		c.late = nil
	}
}

func (c *Coordinator) onLateTimer(m msg) {
	if c.late != nil && c.late.gen == m.gen {
		c.d.Logger.Debug("late CallActive window closed", "call_id", c.late.id)
		c.clearLate()
	}
}

// onTelIdle handles phone events while no call is in progress.
func (c *Coordinator) onTelIdle(ev hfp.Event) {
	w := c.late
	if w == nil {
		return
	}
	switch ev.Kind {
	case hfp.CallActive:
		w.callActive = true
		c.resolveLate()
	case hfp.CallSetupCleared, hfp.CallEnded:
		c.clearLate()
	}
}

// ---- SCO ----

func (c *Coordinator) startSCO(call *activeCall) {
	call.acceptCtx, call.acceptCancel = context.WithCancel(call.ctx)
	c.acceptSCO(call)
	c.startTimer(call, timerSCODial, scoDialAfter)
	c.startTimer(call, timerSCOLimit, scoLimit)
}

func (c *Coordinator) acceptSCO(call *activeCall) {
	lis, ctx, gen := c.listener, call.acceptCtx, call.gen
	if lis == nil {
		return
	}
	c.bg.Add(1) // joined at shutdown before the final inbox drain
	go func() {
		defer c.bg.Done()
		defer c.recoverGo(gen)
		l, err := lis.Accept(ctx)
		c.postLink(gen, l, err)
	}()
}

func (c *Coordinator) dialSCO(call *activeCall) {
	d := c.d.Dialer
	if d == nil {
		return
	}
	ctx, gen, adapter, phone := call.acceptCtx, call.gen, c.adapterAddr, c.d.Config.Bluetooth.PhoneAddress
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer c.recoverGo(gen)
		l, err := d.Dial(ctx, adapter, phone)
		c.postLink(gen, l, err)
	}()
}

func (c *Coordinator) postLink(gen int, l sco.Link, err error) {
	if !c.post(msg{kind: mSCO, gen: gen, link: l, err: err}) && l != nil {
		_ = l.Close()
	}
}

// onCallTimer handles the timers of Answering and Bridged.
func (c *Coordinator) onCallTimer(call *activeCall, name string) {
	switch name {
	case timerCallActive:
		if call.state == stateAnswering && !call.callActive {
			if call.setupCleared {
				c.finish(call, endSpec{outcome: OutcomeCallerAbandoned, hangupBy: HangupCaller})
				return
			}
			c.finish(call, endSpec{outcome: OutcomeAnswerFailed, hangupBy: HangupSiptooth, errText: "no CallActive after ATA"})
		}
	case timerSCODial:
		if call.state == stateAnswering && call.link == nil {
			c.d.Logger.Info("no SCO from the phone, dialing", "call_id", call.id)
			c.dialSCO(call)
		}
	case timerSCOLimit:
		if call.state == stateAnswering && call.link == nil {
			c.finish(call, endSpec{outcome: OutcomeAnswerFailed, hangupBy: HangupSiptooth, hangupPhone: true, errText: "no SCO link"})
		}
	case timerMediaPoll:
		c.onMediaPoll(call)
	default:
		c.onBridgedTimer(call, name)
	}
}

func (c *Coordinator) onSCO(m msg) {
	call := c.current(m.gen)
	if call == nil || call.state != stateAnswering || call.link != nil || !call.callActive {
		c.discard(m, "not waiting for SCO")
		return
	}
	if m.err != nil || m.link == nil {
		if !errors.Is(m.err, context.Canceled) {
			c.d.Logger.Warn("SCO setup failed", "call_id", call.id, "error", m.err)
		}
		return
	}
	link := m.link
	if phone := c.d.Config.Bluetooth.PhoneAddress; phone != "" && link.Remote() != "" && !strings.EqualFold(link.Remote(), phone) {
		c.d.Logger.Warn("SCO from an unexpected device ignored", "call_id", call.id, "remote", link.Remote())
		_ = link.Close()
		c.acceptSCO(call) // keep listening for the phone
		return
	}
	call.link = link
	call.acceptCancel()
	c.stopTimer(call, timerSCODial)
	c.stopTimer(call, timerSCOLimit)
	// With in-band ringing the link was accepted while the phone rang and has
	// queued ringtone ever since; drop it before the first paced write.
	backlog, err := sco.Drain(link)
	if err != nil {
		c.d.Logger.Warn("SCO backlog drain failed", "call_id", call.id, "error", err)
	}
	now := c.d.Clock.Now()
	call.state = stateBridged
	call.bridgedAt = now
	close(call.silenceStop)
	c.d.Logger.Info("sco up", "call_id", call.id, "mtu", link.MTU(), "remote", link.Remote(), "backlog_frames", backlog)

	info := c.callInfo(call)
	info.ConnectedAt = &now
	c.d.Hooks.Emit("call.connected", info)
	c.startBridged(call)
}

// ---- media supervision ----

// onMediaPoll ends the call when no RTP arrived for calls.media_timeout_seconds
// (counted from the media start until the first packet).
func (c *Coordinator) onMediaPoll(call *activeCall) {
	if call.state != stateAnswering && call.state != stateBridged {
		return
	}
	c.startTimer(call, timerMediaPoll, mediaPollEvery)
	limit := time.Duration(c.d.Config.Calls.MediaTimeoutSeconds) * time.Second
	if limit <= 0 || call.draining {
		return
	}
	ref := call.prepared.Media.Stats().LastRx
	if ref.Before(call.mediaStartedAt) {
		ref = call.mediaStartedAt
	}
	if idle := c.d.Clock.Now().Sub(ref); idle >= limit {
		c.finish(call, endSpec{outcome: OutcomeMediaTimeout, hangupBy: HangupSiptooth, hangupPhone: call.ours(),
			errText: fmt.Sprintf("no RTP for %s", idle.Round(time.Second))})
	}
}

func (c *Coordinator) onMediaErr(m msg) {
	call := c.current(m.gen)
	if call == nil || (call.state != stateAnswering && call.state != stateBridged) {
		c.discard(m, "call over")
		return
	}
	c.finish(call, endSpec{outcome: OutcomeMediaError, hangupBy: HangupSiptooth, hangupPhone: call.ours(), errText: "rtp: " + errString(m.err)})
}

func (c *Coordinator) onMediaUpdate(m msg) {
	call := c.current(m.gen)
	if call == nil || (call.state != stateAnswering && call.state != stateBridged) {
		c.discard(m, "call over")
		return
	}
	c.d.Logger.Info("remote media updated", "call_id", call.id, "remote", m.media.Addr.String())
	call.prepared.Media.Update(m.media)
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// recoverGo contains a panic in a call goroutine: the loop tears the call down.
func (c *Coordinator) recoverGo(gen int) {
	if r := recover(); r != nil {
		c.post(msg{kind: mPanic, gen: gen, err: fmt.Errorf("panic: %v", r)})
	}
}
