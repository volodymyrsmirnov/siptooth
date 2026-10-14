package sip

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
)

// DialRequest describes one outgoing call (spec 5.2). The strings arrive
// already expanded and escaped by internal/placeholder.
type DialRequest struct {
	Target      string // Request-URI and To, a sip: URI
	From        string // From URI
	DisplayName string // quoted string as placeholder.ExpandDisplayName returns it; may be empty
	Contact     string // signaling address URI (spec 7.3), required
	Headers     map[string]string
	Offer       sdp.Offer

	// NextHop ("host:port") and Transport ("udp"|"tcp"|"tls") default to the
	// UA's. The next hop is where the INVITE is sent; it need not be the
	// host of Target (sip.proxy).
	NextHop   string
	Transport string

	// OnResponse, if not nil, is called for every response to the INVITE the
	// attempt reads, including retransmissions it passes up. It runs on the
	// attempt's goroutine and must not block.
	OnResponse func(status int, reason string)
}

// FailCause classifies why a dial did not produce a call (spec 4.3).
type FailCause string

const (
	FailRejected  FailCause = "sip_rejected"     // final 3xx-6xx
	FailError     FailCause = "sip_error"        // DNS, connect, TLS, auth, transaction
	FailMedia     FailCause = "sip_media_failed" // 2xx with unacceptable SDP
	FailAbandoned FailCause = "abandoned"        // Abandon was called first
)

// DialResult is the single outcome of an Attempt. Call is set (and Cause
// empty) when the far end answered with acceptable SDP.
type DialResult struct {
	Call   Call
	Cause  FailCause
	Status int    // last SIP status of the INVITE, 0 if none
	Reason string // reason phrase, or "auth_required", "auth_failed", ...
	Err    error
}

// Attempt is one INVITE in flight. Neither method ever blocks.
type Attempt interface {
	// Result delivers exactly one value.
	Result() <-chan DialResult
	// Abandon gives up on the attempt. The result (FailAbandoned unless a
	// result was already published) is published at once; CANCEL, or
	// ACK plus BYE for an answer that crosses it, happen in the background.
	// Idempotent.
	Abandon()
}

var errAbandonedAtChallenge = errors.New("sip: abandoned before the authenticated retry")

// bye/ACK cleanup after an unwanted 2xx is bounded by this (spec 5.3).
const cleanupTimeout = 5 * time.Second

// registryLinger is how long the UA keeps routing late 2xx responses of a
// finished attempt (forked answers) to it: transaction Timer M, 64*T1.
var registryLinger = 64 * gosip.T1

// Start sends an INVITE and returns at once; everything that can block
// (DNS, connect, TLS, the transaction) runs on the attempt's goroutine. The
// error covers an invalid request or a closed UA only.
func (u *UA) Start(req DialRequest) (Attempt, error) {
	transport := strings.ToLower(req.Transport)
	if transport == "" {
		transport = u.transport
	}
	switch transport {
	case "udp", "tcp", "tls":
	default:
		return nil, fmt.Errorf("sip: unsupported transport %q", req.Transport)
	}
	hop := req.NextHop
	if hop == "" {
		hop = u.opts.NextHop
	}
	invite, err := buildInvite(req, strings.ToUpper(transport), hop, u.opts.UserAgent)
	if err != nil {
		return nil, err
	}

	a := &attempt{
		u:       u,
		req:     req,
		invite:  invite,
		callID:  invite.CallID().Value(),
		result:  make(chan DialResult, 1),
		claimed: make(map[string]bool),
		pending: make(map[string]*gosip.Response),
	}
	a.setupCtx, a.setupCancel = context.WithCancel(u.bgCtx)
	a.waitCtx, a.waitCancel = context.WithCancel(context.Background())

	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		a.setupCancel()
		a.waitCancel()
		return nil, errors.New("sip: user agent closed")
	}
	u.attempts[a.callID] = a
	u.mu.Unlock()

	if !u.spawn(a.run) {
		u.forget(a.callID)
		a.setupCancel()
		a.waitCancel()
		return nil, errors.New("sip: user agent closed")
	}
	return a, nil
}

// forgetLater drops the attempt from the registry after registryLinger. The
// timer is tracked so Close can stop it.
func (u *UA) forgetLater(callID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(registryLinger, func() {
		u.mu.Lock()
		delete(u.timers, t)
		delete(u.attempts, callID)
		u.mu.Unlock()
	})
	u.timers[t] = struct{}{}
}

func (u *UA) forget(callID string) {
	u.mu.Lock()
	delete(u.attempts, callID)
	u.mu.Unlock()
}

// attempt owns one INVITE transaction from the first byte to the last ACK or
// BYE it is responsible for (spec 5.3).
type attempt struct {
	u      *UA
	req    DialRequest
	invite *gosip.Request
	callID string
	result chan DialResult // capacity 1; written once under mu

	// setupCtx covers DNS, connect and TLS before the INVITE is on the wire;
	// Abandon cancels it then. waitCtx makes sipgo's WaitAnswer send CANCEL;
	// Abandon cancels it only after a 1xx has been seen (RFC 3261 9.1).
	setupCtx    context.Context
	setupCancel context.CancelFunc
	waitCtx     context.Context
	waitCancel  context.CancelFunc

	mu        sync.Mutex
	abandoned bool
	sent      bool // the INVITE transaction exists
	seen1xx   bool
	published bool
	// decided is set once the goroutine knows which dialog (To tag) it keeps
	// or that it keeps none. claimed holds the tags somebody owns (kept, or
	// cleaned up once); pending holds 2xx that arrived before the decision.
	decided bool
	claimed map[string]bool
	pending map[string]*gosip.Response
}

func (a *attempt) Result() <-chan DialResult { return a.result }

func (a *attempt) Abandon() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.abandoned {
		return
	}
	a.abandoned = true
	a.publishLocked(DialResult{Cause: FailAbandoned})
	switch {
	case !a.sent:
		a.setupCancel() // nothing on the wire yet; stop connecting
	case a.seen1xx:
		a.waitCancel() // CANCEL now
	}
	// else: the first 1xx (onResponse) or a final response ends it.
}

// publishLocked delivers r unless a result was already delivered. a.mu held.
func (a *attempt) publishLocked(r DialResult) bool {
	if a.published {
		return false
	}
	a.published = true
	a.result <- r
	return true
}

func (a *attempt) publish(r DialResult) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.publishLocked(r)
}

func (a *attempt) isAbandoned() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.abandoned
}

func (a *attempt) hasCredentials() bool {
	return a.u.opts.Username != "" && a.u.opts.Password != ""
}

func (a *attempt) run() {
	u := a.u
	defer a.setupCancel()
	defer a.waitCancel()
	// A late 2xx of a fork still needs routing for one transaction lifetime.
	defer u.forgetLater(a.callID)

	LogMessage(u.log, "send", a.invite)
	sess, err := u.dialogs.WriteInvite(a.setupCtx, a.invite)
	if err != nil {
		a.cleanup(a.decide(""))
		a.publish(DialResult{Cause: FailError, Reason: "invite_failed", Err: fmt.Errorf("sip: send INVITE: %w", err)})
		return
	}
	a.mu.Lock()
	a.sent = true
	a.mu.Unlock()

	opts := sipgo.AnswerOptions{OnResponse: a.onResponse}
	if a.hasCredentials() {
		opts.Username, opts.Password = u.opts.Username, u.opts.Password
	}
	werr := sess.WaitAnswer(a.waitCtx, opts)

	// WaitAnswer may return an error (context canceled, "non provisional
	// response received during CANCEL") with a 2xx sitting un-ACKed in
	// InviteResponse. Look at it whatever the error was.
	if res := sess.InviteResponse; res != nil && res.IsSuccess() {
		a.answered(sess, res, werr)
		return
	}
	a.cleanup(a.decide(""))
	a.publish(a.classify(sess, werr))
}

func (a *attempt) onResponse(res *gosip.Response) error {
	if f := a.req.OnResponse; f != nil {
		f(res.StatusCode, res.Reason)
	}
	switch {
	case res.IsProvisional():
		a.mu.Lock()
		a.seen1xx = true
		if a.abandoned {
			a.waitCancel() // the first 1xx after Abandon: CANCEL now
		}
		a.mu.Unlock()
	case isChallenge(res) && a.hasCredentials():
		// sipgo re-sends the INVITE as a new transaction. Provisionals of
		// the old one do not allow a CANCEL of the new one (RFC 3261 9.1),
		// and an abandoned attempt must not retry at all.
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.abandoned {
			return errAbandonedAtChallenge
		}
		a.seen1xx = false
	}
	return nil
}

// classify maps a WaitAnswer error without a 2xx to a result (spec 4.3, 5.4).
func (a *attempt) classify(sess *sipgo.DialogClientSession, err error) DialResult {
	var res *gosip.Response
	var ptr *sipgo.ErrDialogResponse
	var val sipgo.ErrDialogResponse
	switch {
	case errors.As(err, &ptr):
		res = ptr.Res
	case errors.As(err, &val):
		res = val.Res
	}
	if res == nil {
		r := DialResult{Cause: FailError, Reason: "invite_failed", Err: err}
		if last := sess.InviteResponse; last != nil {
			r.Status = last.StatusCode
		}
		return r
	}
	switch res.StatusCode {
	case gosip.StatusUnauthorized, gosip.StatusProxyAuthRequired:
		reason := "auth_required"
		if a.hasCredentials() {
			reason = "auth_failed"
		}
		return DialResult{Cause: FailError, Status: res.StatusCode, Reason: reason,
			Err: fmt.Errorf("sip: INVITE challenged with %d %s: %s", res.StatusCode, res.Reason, reason)}
	}
	return DialResult{Cause: FailRejected, Status: res.StatusCode, Reason: res.Reason}
}

// answered handles a 2xx that WaitAnswer left in sess.InviteResponse.
// werr is WaitAnswer's error: nil means the 2xx was delivered normally.
func (a *attempt) answered(sess *sipgo.DialogClientSession, res *gosip.Response, werr error) {
	a.cleanup(a.decide(toTag(res)))

	status, reason := res.StatusCode, res.Reason
	c := newCall(a.u, sess, a.req.Offer, strings.ToLower(a.invite.Transport()))

	if werr != nil || a.isAbandoned() {
		// Crossed a CANCEL, or abandoned: nobody wants this dialog.
		if !a.isAbandoned() {
			a.publish(DialResult{Cause: FailError, Status: status, Reason: "invite_failed", Err: werr})
		}
		a.ackAndBye(sess, c)
		return
	}
	media, err := sdp.ParseAnswer(res.Body(), a.req.Offer)
	if err != nil {
		a.publish(DialResult{Cause: FailMedia, Status: status, Reason: reason, Err: err})
		a.ackAndBye(sess, c)
		return
	}
	if err := sess.Ack(a.u.bgCtx); err != nil {
		a.publish(DialResult{Cause: FailError, Status: status, Reason: "ack_failed", Err: fmt.Errorf("sip: send ACK: %w", err)})
		a.byeDirect(c, sess)
		return
	}
	c.setAnswer(media, status, reason, res)
	a.mu.Lock()
	ok := !a.abandoned && a.publishLocked(DialResult{Call: c, Status: status, Reason: reason})
	a.mu.Unlock()
	if !ok {
		// Abandon won the race after the ACK: the dialog is established, end it.
		a.byeOnly(c)
		return
	}
	c.start()
}

// ackAndBye ACKs the 2xx and ends the dialog, bounded by cleanupTimeout.
func (a *attempt) ackAndBye(sess *sipgo.DialogClientSession, c *call) {
	if err := sess.Ack(a.u.bgCtx); err != nil {
		a.u.log.Debug("ACK for discarded answer failed", "error", err)
		a.byeDirect(c, sess)
		return
	}
	a.byeOnly(c)
}

// byeDirect ends a dialog whose ACK could not be written: sipgo's WriteBye
// refuses to run unless the dialog is Confirmed, so the BYE is built here.
func (a *attempt) byeDirect(c *call, sess *sipgo.DialogClientSession) {
	c.hangingUp.Store(true)
	c.finish()
	ctx, cancel := context.WithTimeout(a.u.bgCtx, cleanupTimeout)
	defer cancel()
	bye := buildBye(a.invite, sess.InviteResponse, true, a.u.opts.UserAgent)
	if err := a.u.sendBye(ctx, bye); err != nil {
		a.u.log.Debug("BYE without ACK failed", "error", err)
	}
}

func (a *attempt) byeOnly(c *call) {
	ctx, cancel := context.WithTimeout(a.u.bgCtx, cleanupTimeout)
	defer cancel()
	if err := c.Hangup(ctx); err != nil {
		a.u.log.Debug("BYE for discarded answer failed", "error", err)
	}
}

// decide records which To tag the goroutine keeps ("" for none) and returns
// the 2xx that arrived earlier for other tags: those are forks to clean up.
func (a *attempt) decide(tag string) []*gosip.Response {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decided = true
	if tag != "" {
		a.claimed[tag] = true
	}
	var out []*gosip.Response
	for t, r := range a.pending {
		if !a.claimed[t] {
			a.claimed[t] = true
			out = append(out, r)
		}
	}
	a.pending = nil
	return out
}

// onSuccess is called for every 2xx to the INVITE the transport layer
// receives, including the one sipgo passes to WaitAnswer. sipgo v1.6.0
// delivers only the first 2xx of a transaction; any other To tag is a fork
// (RFC 3261 13.2.2.4) and is ACKed and BYEd here.
func (a *attempt) onSuccess(res *gosip.Response) {
	tag := toTag(res)
	if tag == "" {
		return
	}
	a.mu.Lock()
	if a.claimed[tag] {
		a.mu.Unlock() // the kept dialog, or a retransmission of a fork
		return
	}
	if !a.decided {
		a.pending[tag] = res.Clone()
		a.mu.Unlock()
		return
	}
	a.claimed[tag] = true
	a.mu.Unlock()
	a.cleanup([]*gosip.Response{res})
}

// cleanup ACKs and BYEs each response's dialog in the background.
func (a *attempt) cleanup(forks []*gosip.Response) {
	for _, res := range forks {
		res := res
		a.u.spawn(func() { a.cleanupFork(res) })
	}
}

func (a *attempt) cleanupFork(res *gosip.Response) {
	u := a.u
	ctx, cancel := context.WithTimeout(u.bgCtx, cleanupTimeout)
	defer cancel()
	ack := buildAck(a.invite, res)
	LogMessage(u.log, "send", ack)
	if err := u.client.WriteRequest(ack); err != nil {
		u.log.Debug("ACK for extra answer failed", "error", err)
		return
	}
	bye := buildBye(a.invite, res, true, u.opts.UserAgent)
	LogMessage(u.log, "send", bye)
	if err := u.sendBye(ctx, bye); err != nil {
		u.log.Debug("BYE for extra answer failed", "error", err)
	}
}

// onMessage is the transport layer tap: it logs and routes 2xx responses of
// INVITEs to their attempt.
func (u *UA) onMessage(m gosip.Message) {
	LogMessage(u.log, "recv", m)
	res, ok := m.(*gosip.Response)
	if !ok || !res.IsSuccess() {
		return
	}
	if cs := res.CSeq(); cs == nil || cs.MethodName != gosip.INVITE {
		return
	}
	id := res.CallID()
	if id == nil {
		return
	}
	u.mu.Lock()
	a := u.attempts[id.Value()]
	u.mu.Unlock()
	if a != nil {
		a.onSuccess(res)
	}
}

// waitFinal returns the first final response of tx.
func waitFinal(ctx context.Context, tx gosip.ClientTransaction) (*gosip.Response, error) {
	for {
		select {
		case res := <-tx.Responses():
			if res.IsProvisional() {
				continue
			}
			return res, nil
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("sip: transaction terminated")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// isChallenge reports whether res asks for digest credentials.
func isChallenge(res *gosip.Response) bool {
	return res.StatusCode == gosip.StatusUnauthorized || res.StatusCode == gosip.StatusProxyAuthRequired
}

// sendBye sends bye outside any sipgo session and waits for the final
// answer, answering one digest challenge (spec 5.4).
func (u *UA) sendBye(ctx context.Context, bye *gosip.Request) error {
	tx, err := u.client.TransactionRequest(ctx, bye)
	if err != nil {
		return err
	}
	defer tx.Terminate()
	res, err := waitFinal(ctx, tx)
	if err != nil {
		return err
	}
	return u.byeResult(ctx, bye, res)
}

// byeResult turns the first final response to bye into an error or nil,
// re-sending bye once with credentials when it was challenged.
func (u *UA) byeResult(ctx context.Context, bye *gosip.Request, res *gosip.Response) error {
	if res.IsSuccess() {
		return nil
	}
	if !isChallenge(res) || u.opts.Username == "" || u.opts.Password == "" {
		return fmt.Errorf("sip: BYE answered %d %s", res.StatusCode, res.Reason)
	}
	tx, err := u.client.TransactionDigestAuth(ctx, bye, res, sipgo.DigestAuth{
		Username: u.opts.Username, Password: u.opts.Password,
	})
	if err != nil {
		return fmt.Errorf("sip: BYE digest: %w", err)
	}
	defer tx.Terminate()
	res, err = waitFinal(ctx, tx)
	if err != nil {
		return err
	}
	if !res.IsSuccess() {
		return fmt.Errorf("sip: BYE answered %d %s after authentication", res.StatusCode, res.Reason)
	}
	return nil
}
