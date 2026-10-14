package sip

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
)

// installHandlers registers the far-end requests this layer answers (spec
// 5.5, 5.6): BYE and re-INVITE of a dialog siptooth created, ACK, OPTIONS,
// and 403/603/481/501 for everything else. Siptooth never answers a new
// incoming call.
func (u *UA) installHandlers() {
	u.server.OnBye(u.handleBye)
	u.server.OnAck(u.handleAck)
	u.server.OnOptions(u.handleOptions)
	u.server.OnInvite(u.handleInvite)
	u.server.OnNoRoute(func(req *gosip.Request, tx gosip.ServerTransaction) {
		u.reply(req, tx, gosip.StatusNotImplemented, "Not Implemented")
	})
}

// reply answers req with a bodyless response.
func (u *UA) reply(req *gosip.Request, tx gosip.ServerTransaction, code int, reason string) {
	res := gosip.NewResponseFromRequest(req, code, reason, nil)
	LogMessage(u.log, "send", res)
	if err := tx.Respond(res); err != nil {
		u.log.Debug("response failed", "method", req.Method.String(), "status", code, "error", err)
	}
}

func (u *UA) handleBye(req *gosip.Request, tx gosip.ServerTransaction) {
	if sess, err := u.dialogs.MatchRequestDialog(req); err == nil {
		u.mu.Lock()
		c := u.calls[sess]
		u.mu.Unlock()
		if c != nil {
			c.byeSeen.Store(true)
		}
	}
	err := u.dialogs.ReadBye(req, tx)
	if err == nil {
		return
	}
	if errors.Is(err, sipgo.ErrDialogDoesNotExists) || errors.Is(err, sipgo.ErrDialogOutsideDialog) {
		u.reply(req, tx, gosip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist")
		return
	}
	u.log.Debug("BYE handling failed", "error", err)
}

// callFor returns the live call of the dialog req belongs to, or nil.
func (u *UA) callFor(req *gosip.Request) *call {
	sess, err := u.dialogs.MatchRequestDialog(req)
	if err != nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[sess]
}

func (u *UA) handleAck(req *gosip.Request, _ gosip.ServerTransaction) {
	if c := u.callFor(req); c != nil {
		c.onAck(req)
	}
}

func (u *UA) handleOptions(req *gosip.Request, tx gosip.ServerTransaction) {
	res := gosip.NewResponseFromRequest(req, gosip.StatusOK, "OK", nil)
	res.AppendHeader(gosip.NewHeader("Allow", allowMethods))
	LogMessage(u.log, "send", res)
	if err := tx.Respond(res); err != nil {
		u.log.Debug("OPTIONS response failed", "error", err)
	}
}

// handleInvite serves re-INVITEs of a live call; any other INVITE is refused:
// 481 when it claims to be in a dialog, 603 when it is meant for the
// registered Contact (someone calling the extension), else 403.
func (u *UA) handleInvite(req *gosip.Request, tx gosip.ServerTransaction) {
	if c := u.callFor(req); c != nil {
		c.handleReinvite(req, tx)
		return
	}
	if to := req.To(); to != nil && to.Params.Has("tag") {
		u.reply(req, tx, gosip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist")
		return
	}
	if r := u.reg.Load(); r != nil && r.addressedBy(req) {
		u.reply(req, tx, 603, "Decline")
		return
	}
	u.reply(req, tx, gosip.StatusForbidden, "Forbidden")
}

// reinvitePending is a re-INVITE whose 2xx still waits for its ACK.
type reinvitePending struct {
	cseq      uint32
	offerless bool
	next      *sdp.Media // accepted address change, applied at the ACK
	acked     chan struct{}
}

func (c *call) handleReinvite(req *gosip.Request, tx gosip.ServerTransaction) {
	u := c.u
	cseq := req.CSeq().SeqNo

	c.mu.Lock()
	if c.haveCSeq && cseq <= c.remoteCSeq {
		c.mu.Unlock()
		u.reply(req, tx, gosip.StatusInternalServerError, "Server Internal Error")
		return
	}
	c.remoteCSeq, c.haveCSeq = cseq, true

	p := &reinvitePending{cseq: cseq, acked: make(chan struct{})}
	if len(bytes.TrimSpace(req.Body())) == 0 {
		p.offerless = true
	} else {
		kind, next, err := sdp.ClassifyReoffer(req.Body(), c.media, c.offer)
		if err != nil || kind == sdp.Reject {
			c.mu.Unlock()
			u.log.Info("re-INVITE refused", "call_id", c.SIPCallID(), "error", err)
			u.reply(req, tx, gosip.StatusNotAcceptableHere, "Not Acceptable Here")
			return
		}
		if kind == sdp.AddressChange {
			p.next = &next
		}
	}
	c.offer.Version++
	body := sdp.AnswerFor(c.offer, c.media)
	c.pendingAck = p
	// A re-INVITE is a target refresh: its Contact is where our requests go
	// from now on; the route set stays (RFC 3261 12.2.2).
	if ct := req.Contact(); ct != nil && ct.Address.Host != "" {
		c.path.target = *ct.Address.Clone()
	}
	c.mu.Unlock()

	res := gosip.NewResponseFromRequest(req, gosip.StatusOK, "OK", body)
	res.AppendHeader(c.localContact())
	ct := gosip.ContentTypeHeader("application/sdp")
	res.AppendHeader(&ct)
	LogMessage(u.log, "send", res)
	if err := tx.Respond(res); err != nil {
		u.log.Debug("re-INVITE 200 failed", "call_id", c.SIPCallID(), "error", err)
		c.clearPending(p)
		return
	}
	u.spawn(func() { c.awaitAck(res, p) })
}

// localContact is the Contact of our answers: the registered Contact while
// registered (so the far end keeps using the flow the registrar knows),
// otherwise the one the INVITE carried.
func (c *call) localContact() gosip.Header {
	if r := c.u.reg.Load(); r != nil {
		if st := r.State(); st.Registered {
			return &gosip.ContactHeader{Address: r.contactURI(st.Contact)}
		}
	}
	return gosip.HeaderClone(c.invite.Contact())
}

func (c *call) clearPending(p *reinvitePending) {
	c.mu.Lock()
	if c.pendingAck == p {
		c.pendingAck = nil
	}
	c.mu.Unlock()
}

// awaitAck retransmits the 2xx on every transport (T1 doubling to T2, RFC
// 3261 13.3.1.4) until the ACK arrives. After reinviteAckTimeout without one
// the far end is gone: BYE and TransportLost (spec 5.6).
func (c *call) awaitAck(res *gosip.Response, p *reinvitePending) {
	u := c.u
	deadline := time.NewTimer(u.reinviteAckTimeout)
	defer deadline.Stop()
	interval := u.reinviteT1
	for {
		t := time.NewTimer(interval)
		tick := t.C
		select {
		case <-p.acked:
		case <-c.ctx.Done():
		case <-deadline.C:
			c.mu.Lock()
			current := c.pendingAck == p
			c.mu.Unlock()
			if current {
				u.log.Warn("no ACK for re-INVITE answer, ending call", "call_id", c.SIPCallID())
				c.end(TransportLost)
				c.byeAfterLoss()
			}
		case <-tick:
			c.mu.Lock()
			current := c.pendingAck == p
			c.mu.Unlock()
			if !current {
				return // a newer re-INVITE took over
			}
			if err := u.ua.TransportLayer().WriteMsg(res.Clone()); err != nil {
				u.log.Debug("2xx retransmission failed", "call_id", c.SIPCallID(), "error", err)
			}
			if interval *= 2; interval > u.reinviteT2 {
				interval = u.reinviteT2
			}
			continue
		}
		t.Stop()
		return
	}
}

// byeAfterLoss tells the far end the dialog is over, bounded by cleanupTimeout.
func (c *call) byeAfterLoss() {
	ctx, cancel := context.WithTimeout(c.u.bgCtx, cleanupTimeout)
	defer cancel()
	if err := c.Hangup(ctx); err != nil {
		c.u.log.Debug("BYE after missing ACK failed", "error", err)
	}
}

// onAck completes a pending re-INVITE: an accepted address change (or the
// SDP answer of an offerless one) is applied and published now.
func (c *call) onAck(ack *gosip.Request) {
	cs := ack.CSeq()
	if cs == nil {
		return
	}
	c.mu.Lock()
	p := c.pendingAck
	if p == nil || p.cseq != cs.SeqNo {
		c.mu.Unlock()
		return // ACK of the initial INVITE or a stale one
	}
	c.pendingAck = nil
	close(p.acked)

	next := p.next
	if p.offerless {
		next = nil
		if len(bytes.TrimSpace(ack.Body())) == 0 {
			c.u.log.Warn("ACK of an offerless re-INVITE has no SDP", "call_id", c.SIPCallID())
		} else if kind, m, err := sdp.ClassifyReoffer(ack.Body(), c.media, c.offer); err != nil || kind == sdp.Reject {
			c.u.log.Warn("SDP in ACK refused, keeping current media", "call_id", c.SIPCallID(), "error", err)
		} else if kind == sdp.AddressChange {
			next = &m
		}
	}
	if next == nil {
		c.mu.Unlock()
		return
	}
	c.media = *next
	m := *next
	c.mu.Unlock()
	c.publishMedia(m)
}

// publishMedia delivers an accepted address change; when the consumer is
// behind, the oldest pending update is dropped (only the latest matters).
func (c *call) publishMedia(m sdp.Media) {
	for {
		select {
		case c.mediaUpdates <- m:
			return
		default:
		}
		select {
		case <-c.mediaUpdates:
		default:
		}
	}
}

// addressedBy reports whether req is meant for this registration: its
// Request-URI names our user or our registered Contact.
func (r *Registration) addressedBy(req *gosip.Request) bool {
	st := r.State()
	if !st.Registered {
		return false
	}
	uri := req.Recipient
	if r.aor.User != "" && uri.User == r.aor.User {
		return true
	}
	if uri.Port != 0 {
		return net.JoinHostPort(uri.Host, strconv.Itoa(uri.Port)) == st.Contact.String()
	}
	return false
}
