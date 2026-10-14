package sip

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
)

// EndCause says why a Call ended without a local Hangup.
type EndCause string

const (
	RemoteBye     EndCause = "remote_bye"
	TransportLost EndCause = "transport_lost"
)

// Call is an answered, acknowledged dialog (spec 3.2).
type Call interface {
	// Remote is the negotiated remote media.
	Remote() sdp.Media
	// MediaUpdates delivers accepted re-INVITE address changes (spec 5.6).
	MediaUpdates() <-chan sdp.Media
	// Ended delivers one value when the far end hangs up or the signaling
	// connection is lost. A local Hangup delivers nothing.
	Ended() <-chan EndCause
	// Hangup sends BYE, answering one digest challenge, bounded by ctx. Safe
	// to call more than once and after the call ended.
	Hangup(ctx context.Context) error
	SIPCallID() string
	// LastStatus is the status line of the last response to the INVITE.
	LastStatus() (int, string)
}

type call struct {
	u         *UA
	sess      *sipgo.DialogClientSession
	offer     sdp.Offer
	transport string // effective transport of this call, lower case
	byeSeen   atomic.Bool

	mu     sync.Mutex
	media  sdp.Media
	status int
	reason string
	hop    string // remote address of the connection that carried the 2xx

	mediaUpdates chan sdp.Media
	ended        chan EndCause
	ctx          context.Context
	stop         context.CancelFunc

	hangingUp atomic.Bool
	hangOnce  sync.Once
	hangDone  chan struct{}
	hangErr   error
}

func newCall(u *UA, sess *sipgo.DialogClientSession, offer sdp.Offer, transport string) *call {
	c := &call{
		u:            u,
		sess:         sess,
		offer:        offer,
		transport:    transport,
		mediaUpdates: make(chan sdp.Media, 4),
		ended:        make(chan EndCause, 1),
		hangDone:     make(chan struct{}),
	}
	c.ctx, c.stop = context.WithCancel(u.bgCtx)
	// A BYE read by the server handler ends the dialog from outside.
	sess.OnState(func(s gosip.DialogState) {
		if s != gosip.DialogStateEnded || c.hangingUp.Load() {
			return
		}
		// sipgo also ends a dialog on local failures (ACK write error,
		// retransmitted ACK failing): only a received BYE is RemoteBye.
		if c.byeSeen.Load() {
			c.end(RemoteBye)
		} else {
			c.end(TransportLost)
		}
	})
	u.mu.Lock()
	u.calls[sess] = c
	u.mu.Unlock()
	return c
}

func (c *call) setAnswer(m sdp.Media, status int, reason string, res *gosip.Response) {
	c.mu.Lock()
	c.media, c.status, c.reason, c.hop = m, status, reason, res.Source()
	c.mu.Unlock()
}

func (c *call) Remote() sdp.Media {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.media
}

func (c *call) MediaUpdates() <-chan sdp.Media { return c.mediaUpdates }
func (c *call) Ended() <-chan EndCause         { return c.ended }
func (c *call) SIPCallID() string              { return c.sess.InviteRequest.CallID().Value() }

func (c *call) LastStatus() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, c.reason
}

// end reports cause once and stops the keepalive.
func (c *call) end(cause EndCause) {
	c.finish()
	select {
	case c.ended <- cause:
	default:
	}
	c.stop()
}

// finish stops the keepalive and drops the call from the UA's BYE routing.
func (c *call) finish() {
	c.stop()
	c.u.mu.Lock()
	delete(c.u.calls, c.sess)
	c.u.mu.Unlock()
}

// start begins the signaling keepalive for connection-oriented transports
// (spec 5.5): a failed ping means the connection the far end would use for
// in-dialog requests is gone.
func (c *call) start() {
	if c.transport == "udp" || c.u.callKeepalive <= 0 {
		return
	}
	c.mu.Lock()
	hop := c.hop
	c.mu.Unlock()
	c.u.spawn(func() {
		keepaliveLoop(c.ctx, c.u.callKeepalive,
			func() error { return c.u.pingConnection(c.transport, hop) },
			func(error) { c.end(TransportLost) })
	})
}

var errConnectionGone = errors.New("sip: signaling connection is gone")

// pingConnection writes a CRLF ping on the pooled connection to remote. A
// call's connection missing from the pool counts as a failure, unlike the
// idle keepalive of StartKeepalive.
func (u *UA) pingConnection(transport, remote string) error {
	conn, err := u.ua.TransportLayer().GetConnection(transport, remote)
	if err != nil || conn == nil {
		return errConnectionGone
	}
	defer conn.TryClose()
	var raddr netip.AddrPort
	if transport == "udp" {
		raddr, _ = netip.ParseAddrPort(remote)
	}
	return writeKeepalive(conn, raddr)
}

// Hangup sends BYE for the call.
func (c *call) Hangup(ctx context.Context) error {
	c.hangOnce.Do(func() {
		go func() {
			defer close(c.hangDone)
			c.hangErr = c.bye(ctx)
		}()
	})
	select {
	case <-c.hangDone:
		return c.hangErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *call) bye(ctx context.Context) error {
	c.hangingUp.Store(true)
	c.finish()
	if c.sess.LoadState() == gosip.DialogStateEnded {
		return nil // the far end already hung up
	}
	u := c.u
	bye := buildBye(c.sess.InviteRequest, c.sess.InviteResponse, false, u.opts.UserAgent)
	LogMessage(u.log, "send", bye)
	err := c.sess.WriteBye(ctx, bye)
	var res *gosip.Response
	var val sipgo.ErrDialogResponse
	var ptr *sipgo.ErrDialogResponse
	switch {
	case errors.As(err, &val):
		res = val.Res
	case errors.As(err, &ptr):
		res = ptr.Res
	}
	if res == nil {
		return err
	}
	return u.byeResult(ctx, bye, res)
}

// installHandlers registers the far-end requests this layer answers: BYE for
// a dialog siptooth created (spec 5.6) and ACK. Everything else is Task 11's.
func (u *UA) installHandlers() {
	u.server.OnBye(func(req *gosip.Request, tx gosip.ServerTransaction) {
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
			if rerr := tx.Respond(gosip.NewResponseFromRequest(req, gosip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)); rerr != nil {
				u.log.Debug("481 for unknown BYE failed", "error", rerr)
			}
			return
		}
		u.log.Debug("BYE handling failed", "error", err)
	})
	u.server.OnAck(func(*gosip.Request, gosip.ServerTransaction) {})
}
