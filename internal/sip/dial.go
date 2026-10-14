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

// ACK, CANCEL and BYE cleanup I/O (DNS, connect, handshake, write) is bounded
// by this (spec 5.3); UA.ioTimeout starts from it.
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
		u:         u,
		req:       req,
		invite:    invite,
		callID:    invite.CallID().Value(),
		hop:       hop,
		transport: transport,
		result:    make(chan DialResult, 1),
		fin:       make(chan struct{}),
		claimed:   make(map[string]bool),
		pending:   make(map[string]*gosip.Response),
		acks:      make(map[string]*gosip.Request),
		forks:     make(map[string]*forkState),
	}
	a.setupCtx, a.setupCancel = context.WithCancel(u.bgCtx)
	// waitCtx covers WaitAnswer, including the connection of an
	// authenticated retry. UA.Close ends it (as a forced cancel, so sipgo
	// does not try a CANCEL of its own).
	a.waitCtx, a.waitCancel = context.WithCancelCause(context.Background())
	a.stopClose = context.AfterFunc(u.bgCtx, func() { a.waitCancel(sipgo.WaitAnswerForceCancelErr) })

	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		a.setupCancel()
		a.waitCancel(nil)
		a.stopClose()
		return nil, errors.New("sip: user agent closed")
	}
	u.attempts[a.callID] = a
	u.mu.Unlock()

	if !u.spawn(a.run) {
		u.forget(a.callID)
		a.setupCancel()
		a.waitCancel(nil)
		a.stopClose()
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
	hop    string // next hop "host:port" the INVITE goes to
	// transport is the effective transport, lower case.
	transport string
	result    chan DialResult // capacity 1; written once under mu
	fin       chan struct{}   // closed when run returns
	stopClose func() bool     // detaches waitCtx from the UA's lifetime

	// setupCtx covers DNS, connect and TLS before the INVITE is on the wire;
	// Abandon cancels it then. waitCtx is WaitAnswer's context; it is only
	// ever cancelled with sipgo.WaitAnswerForceCancelErr, because sipgo's own
	// CANCEL ignores the INVITE's next hop (see sendCancel).
	setupCtx    context.Context
	setupCancel context.CancelFunc
	waitCtx     context.Context
	waitCancel  context.CancelCauseFunc

	mu        sync.Mutex
	abandoned bool
	sent      bool // the INVITE transaction exists
	seen1xx   bool
	// cancelReq is the CANCEL for the current INVITE transaction, built when
	// its first 1xx arrives (RFC 3261 9.1); cancelSent says it went out.
	cancelReq  *gosip.Request
	cancelSent bool
	// acks holds the ACK template of every dialog this attempt ACKed, by To
	// tag; a retransmitted 2xx of that tag is answered with a clone.
	acks map[string]*gosip.Request
	// forks holds the 2xx of every dialog that is being discarded, by To
	// tag, until its BYE has been sent: an ACK that has been delivered always
	// owes a BYE, even when the first ACK failed and a retransmitted 2xx
	// delivers it later.
	forks      map[string]*forkState
	published  bool
	challenges int
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
		a.cancelLocked() // CANCEL now
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
	defer close(a.fin)
	defer a.setupCancel()
	defer a.waitCancel(sipgo.WaitAnswerForceCancelErr)
	defer a.stopClose()
	// A late 2xx of a fork still needs routing for one transaction lifetime.
	defer u.forgetLater(a.callID)

	dest, err := u.destination(a.setupCtx, a.transport, a.hop)
	if err != nil {
		a.cleanup(a.decide(""))
		a.publish(DialResult{Cause: FailError, Reason: "invite_failed", Err: fmt.Errorf("sip: next hop: %w", err)})
		return
	}
	a.invite.SetDestination(dest)

	LogMessage(u.log, "send", a.invite)
	sess, err := u.dialogs.WriteInvite(a.setupCtx, a.invite)
	if err != nil {
		u.forgetHop(a.hop)
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
	// sipgo's WaitAnswer gives up after eleven responses, which a long ring
	// with periodic 180s reaches. Re-enter it while the only problem is that
	// count; challenges are capped in onResponse so this cannot spin on auth.
	var werr error
	for {
		werr = sess.WaitAnswer(a.waitCtx, opts)
		if werr == nil || !strings.Contains(werr.Error(), tooManyResponsesText) || a.waitCtx.Err() != nil {
			break
		}
	}

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

// tooManyResponsesText is the text of sipgo v1.6.0's WaitAnswer error for
// more than ten responses on one call.
const tooManyResponsesText = "more than 10 responses received"

// maxChallenges bounds digest retries across WaitAnswer re-entries.
const maxChallenges = 3

var errTooManyChallenges = errors.New("sip: too many authentication challenges")

func (a *attempt) onResponse(res *gosip.Response) error {
	if f := a.req.OnResponse; f != nil {
		f(res.StatusCode, res.Reason)
	}
	switch {
	case res.IsProvisional():
		a.mu.Lock()
		a.seen1xx = true
		if a.cancelReq == nil {
			a.cancelReq = buildCancel(a.invite, a.u.opts.UserAgent)
		}
		if a.abandoned {
			a.cancelLocked() // the first 1xx after Abandon: CANCEL now
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
		a.challenges++
		if a.challenges > maxChallenges {
			return errTooManyChallenges
		}
		a.seen1xx = false
		a.cancelReq = nil
	}
	return nil
}

// cancelLocked sends the CANCEL of the current INVITE transaction, once, in
// the background. a.mu held; a 1xx has been seen.
func (a *attempt) cancelLocked() {
	if a.cancelSent || a.cancelReq == nil {
		return
	}
	a.cancelSent = true
	req := a.cancelReq
	a.u.spawn(func() { a.sendCancel(req) })
}

// sendCancel sends CANCEL on the INVITE's own next hop and connection (sipgo's
// CANCEL goes to the Request-URI host, past a configured proxy). WaitAnswer
// stays running to read the 487 (or a 2xx that crossed the CANCEL, which
// answered ACKs and BYEs). If the far end never answers the INVITE, WaitAnswer
// is forced out after 64*T1 (RFC 3261 9.1).
func (a *attempt) sendCancel(req *gosip.Request) {
	u := a.u
	ctx, cancel := context.WithTimeout(u.bgCtx, u.ioTimeout)
	LogMessage(u.log, "send", req)
	res, err := u.client.Do(ctx, req)
	cancel()
	switch {
	case err != nil:
		u.log.Debug("CANCEL failed", "call_id", a.callID, "error", err)
	case res.StatusCode != gosip.StatusOK:
		u.log.Debug("CANCEL refused", "call_id", a.callID, "status", res.StatusCode)
	}
	t := time.NewTimer(64 * gosip.T1)
	defer t.Stop()
	select {
	case <-a.fin:
	case <-t.C:
		a.waitCancel(sipgo.WaitAnswerForceCancelErr)
	case <-u.bgCtx.Done():
	}
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
		a.ackAndBye(res, c)
		return
	}
	media, err := sdp.ParseAnswer(res.Body(), a.req.Offer)
	if err != nil {
		a.publish(DialResult{Cause: FailMedia, Status: status, Reason: reason, Err: err})
		a.ackAndBye(res, c)
		return
	}
	if err := a.ack(res); err != nil {
		a.publish(DialResult{Cause: FailError, Status: status, Reason: "ack_failed", Err: fmt.Errorf("sip: send ACK: %w", err)})
		a.byeOnly(c)
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

// ack builds and sends the ACK of a 2xx and remembers it for its To tag, so
// a retransmitted 2xx of that dialog gets an identical ACK (RFC 3261
// 13.2.2.4). The write is bounded by the UA's I/O timeout.
func (a *attempt) ack(res *gosip.Response) error {
	ack := buildAck(a.invite, res)
	a.mu.Lock()
	a.acks[toTag(res)] = ack
	req := ack.Clone() // templates are only touched under mu
	a.mu.Unlock()
	return a.u.sendAck(req)
}

// ackAndBye ACKs the 2xx and ends the dialog, bounded by the I/O timeout.
func (a *attempt) ackAndBye(res *gosip.Response, c *call) {
	if err := a.ack(res); err != nil {
		a.u.log.Debug("ACK for discarded answer failed", "error", err)
	}
	a.byeOnly(c)
}

func (a *attempt) byeOnly(c *call) {
	ctx, cancel := context.WithTimeout(a.u.bgCtx, a.u.ioTimeout)
	defer cancel()
	if err := c.Hangup(ctx); err != nil {
		a.u.log.Debug("BYE for discarded answer failed", "error", err)
	}
}

// sendAck writes an ACK (a clone of the template; the client adds a fresh
// Via), bounded by the I/O timeout: sipgo's own ACK write would dial with no
// deadline.
func (u *UA) sendAck(req *gosip.Request) error {
	ctx, cancel := context.WithTimeout(u.bgCtx, u.ioTimeout)
	defer cancel()
	LogMessage(u.log, "send", req)
	return u.writeRequest(ctx, req)
}

// resendAck answers a retransmitted 2xx of an ACKed dialog.
func (a *attempt) resendAck(tag string) {
	a.mu.Lock()
	tmpl := a.acks[tag]
	var req *gosip.Request
	if tmpl != nil {
		req = tmpl.Clone()
	}
	a.mu.Unlock()
	if req == nil {
		return
	}
	a.u.spawn(func() {
		ctx, cancel := context.WithTimeout(a.u.bgCtx, a.u.ioTimeout)
		defer cancel()
		LogMessage(a.u.log, "send", req)
		if err := a.u.writeRequest(ctx, req); err != nil {
			a.u.log.Debug("ACK for retransmitted 2xx failed", "call_id", a.callID, "error", err)
			return
		}
		a.byeFork(tag) // no-op unless this is a discarded dialog still owing its BYE
	})
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
	if a.acks[tag] != nil {
		a.mu.Unlock() // a retransmitted 2xx of a dialog already ACKed
		a.resendAck(tag)
		return
	}
	if a.claimed[tag] {
		a.mu.Unlock() // the kept dialog before its ACK, or a retransmission of a fork
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

// forkState is a discarded dialog that still owes a BYE.
type forkState struct {
	res     *gosip.Response
	byeSent bool
}

func (a *attempt) cleanupFork(res *gosip.Response) {
	tag := toTag(res)
	a.mu.Lock()
	a.forks[tag] = &forkState{res: res.Clone()}
	a.mu.Unlock()
	if err := a.ack(res); err != nil {
		// A retransmitted 2xx of this tag retries the ACK and, once it is
		// delivered, the BYE (resendAck).
		a.u.log.Debug("ACK for extra answer failed", "error", err)
		return
	}
	a.byeFork(tag)
}

// byeFork sends the BYE of a discarded dialog once its ACK was delivered.
func (a *attempt) byeFork(tag string) {
	u := a.u
	a.mu.Lock()
	f := a.forks[tag]
	if f == nil || f.byeSent {
		a.mu.Unlock()
		return
	}
	f.byeSent = true // one sender at a time; reset if the BYE fails
	res := f.res.Clone()
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(u.bgCtx, u.ioTimeout)
	defer cancel()
	bye := buildBye(a.invite, res, newDialogPath(a.invite, res), u.opts.UserAgent)
	LogMessage(u.log, "send", bye)
	if err := u.sendBye(ctx, bye); err != nil {
		u.log.Debug("BYE for extra answer failed", "error", err)
		a.mu.Lock()
		f.byeSent = false // a further retransmitted 2xx tries again
		a.mu.Unlock()
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
	u.pinRequest(ctx, bye)
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
