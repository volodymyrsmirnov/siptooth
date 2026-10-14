// Package siptest is a fake SIP trunk and media echo for tests of the
// internal/sip package and everything built on it (Tasks 10, 11, 17, 19, 20).
//
// A Trunk is a real sipgo user agent and server listening on 127.0.0.1 over
// UDP, TCP or TLS. It records every request it receives, lets a test script
// its answers per method, and can send in-dialog requests (BYE, re-INVITE)
// back to the caller, either to the caller's Contact or over the connection
// the caller opened. Nothing leaves loopback.
package siptest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Handler answers one received request. tx is nil for requests that have no
// server transaction (ACK for a 2xx).
type Handler = func(req *sip.Request, tx sip.ServerTransaction)

// Header is one header line of a recorded request.
type Header struct {
	Name, Value string
}

// Request is a snapshot of a request the trunk received.
type Request struct {
	Method     string
	RequestURI string
	Headers    []Header
	Body       []byte
	// Source is the remote address the request arrived from.
	Source string
	// Req is a clone of the parsed request, for tests that need typed access.
	Req *sip.Request
}

// Header returns the first value of the named header (case-insensitive), or "".
func (r Request) Header(name string) string {
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// HeaderAll returns every value of the named header (case-insensitive).
func (r Request) HeaderAll(name string) []string {
	var out []string
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

// Trunk is the fake far end. Create it with NewTrunk.
type Trunk struct {
	// ReuseConnection selects how in-dialog requests sent by Dialog reach the
	// caller. True (the default) sends them to the address the INVITE came
	// from, which for TCP/TLS is the connection the caller opened. False sends
	// them to the INVITE's Contact, the way a far end that does not reuse the
	// connection would. Set it before the INVITE arrives; false is meant for
	// UDP, since a TCP/TLS Contact is not a listening address.
	ReuseConnection bool

	t         testing.TB
	transport string
	ua        *sipgo.UserAgent
	client    *sipgo.Client
	server    *sipgo.Server
	addr      netip.AddrPort
	contact   sip.ContactHeader
	ca        *testCA
	closers   []io.Closer
	listener  io.Closer // the TCP/TLS listener, closed by StopListening

	keepalives atomic.Int64
	rawMu      sync.Mutex
	rawMsgs    []string

	mu       sync.Mutex
	requests []Request
	handlers map[string]Handler
	sni      []string
	dialogs  map[string]*Dialog
	cancels  map[string]chan struct{} // by Call-ID of INVITEs seen
}

// serverMethods are the methods routed through the recorder. Anything else
// reaches the same code through OnNoRoute.
var serverMethods = []sip.RequestMethod{
	sip.INVITE, sip.ACK, sip.CANCEL, sip.BYE, sip.OPTIONS, sip.REGISTER,
	sip.INFO, sip.NOTIFY, sip.MESSAGE, sip.REFER, sip.SUBSCRIBE, sip.UPDATE, sip.PRACK,
}

// NewTrunk starts a trunk on 127.0.0.1 with an ephemeral port. transport is
// "udp", "tcp" or "tls". It is shut down by t.Cleanup.
//
// Defaults: OPTIONS answers 200 with an Allow header; ACK and BYE are matched
// against dialogs created with ReadInvite/Accept; every other request is
// recorded and answered 405 until a handler is registered.
func NewTrunk(t testing.TB, transport string) *Trunk {
	t.Helper()
	transport = strings.ToLower(transport)
	tr := &Trunk{
		ReuseConnection: true,
		t:               t,
		transport:       transport,
		handlers:        make(map[string]Handler),
		dialogs:         make(map[string]*Dialog),
		cancels:         make(map[string]chan struct{}),
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	var (
		listener net.Listener
		packet   net.PacketConn
		err      error
	)
	switch transport {
	case "udp":
		packet, err = net.ListenPacket("udp4", "127.0.0.1:0")
		if err == nil {
			tr.addr = packet.LocalAddr().(*net.UDPAddr).AddrPort()
			tr.closers = append(tr.closers, packet)
		}
	case "tcp", "tls":
		listener, err = net.Listen("tcp4", "127.0.0.1:0")
		if err == nil {
			tr.addr = listener.Addr().(*net.TCPAddr).AddrPort()
		}
	default:
		t.Fatalf("siptest: unsupported transport %q", transport)
	}
	if err != nil {
		t.Fatalf("siptest: listen: %v", err)
	}

	if transport == "tls" {
		tr.ca = newTestCA(t)
		certs := map[string]tls.Certificate{
			HostLocalhost: tr.ca.issue(t, 2, []string{HostLocalhost}, []net.IP{net.ParseIP("127.0.0.1")}),
			HostNode1:     tr.ca.issue(t, 3, []string{HostNode1}, nil),
		}
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				tr.mu.Lock()
				tr.sni = append(tr.sni, hi.ServerName)
				tr.mu.Unlock()
				c, ok := certs[strings.ToLower(hi.ServerName)]
				if !ok {
					c = certs[HostLocalhost]
				}
				return &c, nil
			},
		})
	}
	if listener != nil {
		tr.closers = append(tr.closers, listener)
		tr.listener = listener
	}

	tr.ua, err = sipgo.NewUA(
		sipgo.WithUserAgent("siptest-trunk"),
		sipgo.WithUserAgentHostname("127.0.0.1"),
		sipgo.WithUserAgentTransportLayerOptions(
			sip.WithTransportLayerLogger(quiet),
			sip.WithTransportLayerReadFilter(tr.readFilter),
		),
	)
	if err != nil {
		t.Fatalf("siptest: new UA: %v", err)
	}
	tr.client, err = sipgo.NewClient(tr.ua,
		sipgo.WithClientLogger(quiet),
		sipgo.WithClientHostname("127.0.0.1"),
		sipgo.WithClientPort(int(tr.addr.Port())),
	)
	if err != nil {
		t.Fatalf("siptest: new client: %v", err)
	}
	tr.server, err = sipgo.NewServer(tr.ua, sipgo.WithServerLogger(quiet))
	if err != nil {
		t.Fatalf("siptest: new server: %v", err)
	}

	tr.contact = sip.ContactHeader{Address: sip.Uri{
		Scheme: "sip", User: "trunk", Host: "127.0.0.1", Port: int(tr.addr.Port()),
		UriParams: sip.HeaderParams{{K: "transport", V: transport}},
	}}

	for _, m := range serverMethods {
		tr.server.OnRequest(m, tr.route)
	}
	tr.server.OnNoRoute(tr.route)

	switch transport {
	case "udp":
		go tr.server.ServeUDP(packet)
	case "tcp":
		go tr.server.ServeTCP(listener)
	case "tls":
		go tr.server.ServeTLS(listener)
	}

	t.Cleanup(tr.close)
	return tr
}

func (tr *Trunk) close() {
	for _, c := range tr.closers {
		c.Close()
	}
	tr.ua.Close()
}

// StopListening closes the TCP/TLS listener (existing connections stay), so
// a test can bind something else to the same port.
func (tr *Trunk) StopListening() {
	if tr.listener != nil {
		tr.listener.Close()
	}
}

// Transport returns "udp", "tcp" or "tls".
func (tr *Trunk) Transport() string { return tr.transport }

// Addr is the trunk's listen address on 127.0.0.1.
func (tr *Trunk) Addr() netip.AddrPort { return tr.addr }

// HostPort is Addr as "127.0.0.1:port", the form used for Options.NextHop.
func (tr *Trunk) HostPort() string { return tr.addr.String() }

// URI returns a SIP URI for user on this trunk, e.g. "sip:600@127.0.0.1:5060;transport=tls".
func (tr *Trunk) URI(user string) string {
	return fmt.Sprintf("sip:%s@%s;transport=%s", user, tr.addr, tr.transport)
}

// CAPool is the pool that verifies the trunk's TLS certificates (nil for
// udp and tcp).
func (tr *Trunk) CAPool() *x509.CertPool {
	if tr.ca == nil {
		return nil
	}
	return tr.ca.pool
}

// ClientTLSConfig is a client tls.Config trusting the trunk's CA, with no
// ServerName so that sipgo derives it from each destination.
func (tr *Trunk) ClientTLSConfig() *tls.Config {
	return &tls.Config{RootCAs: tr.CAPool(), MinVersion: tls.VersionTLS12}
}

// ServerNames returns the SNI of every TLS handshake the trunk accepted, in
// order (empty string when the client sent none).
func (tr *Trunk) ServerNames() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.sni...)
}

// Keepalives is the number of CRLF keepalive reads (RFC 5626 ping or pong
// sized, one or two CRLFs) the trunk has seen on any connection.
func (tr *Trunk) Keepalives() int { return int(tr.keepalives.Load()) }

func (tr *Trunk) readFilter(_ sip.TransportReadProps, data []byte) ([]byte, error) {
	if len(data) > 0 && len(bytes.Trim(data, "\r\n")) == 0 {
		tr.keepalives.Add(1)
	} else if bytes.HasPrefix(data, []byte("SIP/2.0 ")) {
		tr.rawMu.Lock()
		tr.rawMsgs = append(tr.rawMsgs, string(data))
		tr.rawMu.Unlock()
	}
	return data, nil
}

// RawResponses returns every response datagram or stream chunk the trunk
// read, in order, as received (so retransmitted 2xx show up as copies).
func (tr *Trunk) RawResponses() []string {
	tr.rawMu.Lock()
	defer tr.rawMu.Unlock()
	return append([]string(nil), tr.rawMsgs...)
}

// SendRequest sends an out-of-dialog request with the given method to uri and
// returns the final response. The trunk's own UDP socket is the source, so
// over UDP the target sees the same address as for every other trunk request.
func (tr *Trunk) SendRequest(ctx context.Context, method, uri string, body []byte, headers ...sip.Header) (*sip.Response, error) {
	var target sip.Uri
	if err := sip.ParseUri(uri, &target); err != nil {
		return nil, err
	}
	req := sip.NewRequest(sip.RequestMethod(method), target)
	req.SetTransport(strings.ToUpper(tr.transport))
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: "trunk", Host: "127.0.0.1"}}
	from.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(from)
	for _, h := range headers {
		req.AppendHeader(h)
	}
	if len(body) > 0 {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(body)
	}
	return tr.client.Do(ctx, req)
}

// On registers h for method (any method name, upper case). It replaces a
// previous handler and may be called while the trunk is serving.
func (tr *Trunk) On(method string, h Handler) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.handlers[strings.ToUpper(method)] = h
}

func (tr *Trunk) OnInvite(h Handler)   { tr.On("INVITE", h) }
func (tr *Trunk) OnRegister(h Handler) { tr.On("REGISTER", h) }
func (tr *Trunk) OnOptions(h Handler)  { tr.On("OPTIONS", h) }
func (tr *Trunk) OnBye(h Handler)      { tr.On("BYE", h) }
func (tr *Trunk) OnAck(h Handler)      { tr.On("ACK", h) }
func (tr *Trunk) OnCancel(h Handler)   { tr.On("CANCEL", h) }

func (tr *Trunk) handler(method string) Handler {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.handlers[method]
}

// Requests returns a copy of every request received so far, oldest first.
func (tr *Trunk) Requests() []Request {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]Request(nil), tr.requests...)
}

// RequestsFor returns the received requests of one method.
func (tr *Trunk) RequestsFor(method string) []Request {
	var out []Request
	for _, r := range tr.Requests() {
		if strings.EqualFold(r.Method, method) {
			out = append(out, r)
		}
	}
	return out
}

// WaitRequest waits until the n-th (1-based) request of method has been
// received and returns it; ok is false on timeout.
func (tr *Trunk) WaitRequest(method string, n int, timeout time.Duration) (Request, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if rs := tr.RequestsFor(method); len(rs) >= n {
			return rs[n-1], true
		}
		if time.Now().After(deadline) {
			return Request{}, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (tr *Trunk) record(req *sip.Request) {
	r := Request{
		Method:     req.Method.String(),
		RequestURI: req.Recipient.String(),
		Body:       append([]byte(nil), req.Body()...),
		Source:     req.Source(),
		Req:        req.Clone(),
	}
	for _, h := range req.Headers() {
		r.Headers = append(r.Headers, Header{Name: h.Name(), Value: h.Value()})
	}
	tr.mu.Lock()
	tr.requests = append(tr.requests, r)
	tr.mu.Unlock()
}

// route is the single entry for every request: record, then dispatch.
func (tr *Trunk) route(req *sip.Request, tx sip.ServerTransaction) {
	tr.record(req)
	method := req.Method.String()
	user := tr.handler(method)

	if req.Method == sip.INVITE && tx != nil {
		tr.watchCancel(req, tx)
	}

	switch req.Method {
	case sip.ACK:
		// Confirm the dialog first so Accept/RespondSDP return, then let a
		// test handler see the ACK too.
		if d := tr.dialogFor(req); d != nil {
			d.sess.ReadAck(req, tx)
		}
		if user != nil {
			user(req, tx)
		}
		return
	}
	if user != nil {
		user(req, tx)
		return
	}
	switch req.Method {
	case sip.OPTIONS:
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		res.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS"))
		tx.Respond(res)
	case sip.BYE:
		if d := tr.dialogFor(req); d != nil {
			d.sess.ReadBye(req, tx)
			return
		}
		tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
	default:
		tx.Respond(sip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil))
	}
}

func (tr *Trunk) dialogFor(req *sip.Request) *Dialog {
	id, err := sip.DialogIDFromRequestUAS(req)
	if err != nil {
		return nil
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.dialogs[id]
}

// Dialog is the trunk's side of a call created by an INVITE it received.
type Dialog struct {
	tr   *Trunk
	sess *sipgo.DialogServerSession
}

// ReadInvite turns a received INVITE into a Dialog without answering it. Use
// Respond for provisional answers and RespondSDP to answer 200.
func (tr *Trunk) ReadInvite(req *sip.Request, tx sip.ServerTransaction) (*Dialog, error) {
	ua := &sipgo.DialogUA{Client: tr.client, ContactHDR: tr.contact, RewriteContact: tr.ReuseConnection}
	sess, err := ua.ReadInvite(req, tx)
	if err != nil {
		return nil, err
	}
	d := &Dialog{tr: tr, sess: sess}
	tr.mu.Lock()
	tr.dialogs[sess.ID] = d
	tr.mu.Unlock()
	return d, nil
}

// Accept answers an INVITE with 200 and the given SDP, then waits for the ACK
// (as sipgo does), so it blocks the calling handler until the caller confirms
// the dialog or the transaction fails.
func (tr *Trunk) Accept(req *sip.Request, tx sip.ServerTransaction, sdp []byte) (*Dialog, error) {
	d, err := tr.ReadInvite(req, tx)
	if err != nil {
		return nil, err
	}
	return d, d.RespondSDP(sdp)
}

// Dialogs returns every dialog created so far, in no particular order.
func (tr *Trunk) Dialogs() []*Dialog {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]*Dialog, 0, len(tr.dialogs))
	for _, d := range tr.dialogs {
		out = append(out, d)
	}
	return out
}

// Invite is the INVITE that created the dialog.
func (d *Dialog) Invite() *sip.Request { return d.sess.InviteRequest }

// ID is the sipgo dialog ID.
func (d *Dialog) ID() string { return d.sess.ID }

// State is the dialog state (established after the 200, confirmed after the ACK).
func (d *Dialog) State() sip.DialogState { return d.sess.LoadState() }

// Respond sends a (usually provisional or error) response to the INVITE.
func (d *Dialog) Respond(code int, reason string, body []byte, headers ...sip.Header) error {
	return d.sess.Respond(code, reason, body, headers...)
}

// RespondSDP answers 200 OK with an SDP body and waits for the ACK. A caller
// that sends BYE right behind its ACK can end the dialog before sipgo reads
// the confirmation; that still counts as acknowledged.
func (d *Dialog) RespondSDP(sdp []byte) error {
	err := d.sess.RespondSDP(sdp)
	if err != nil && d.State() >= sip.DialogStateConfirmed {
		return nil
	}
	return err
}

// Bye sends BYE to the caller and waits for its 200.
func (d *Dialog) Bye(ctx context.Context) error { return d.sess.Bye(ctx) }

// ReInvite sends a re-INVITE carrying sdp (nil for an offerless one), waits
// for the final response and ACKs it when it is a 2xx. The response is
// returned so tests can inspect the answer SDP.
func (d *Dialog) ReInvite(ctx context.Context, sdp []byte) (*sip.Response, error) {
	return d.reInvite(ctx, sdp, true, nil)
}

// ReInviteNoAck sends a re-INVITE and returns its final response without
// sending the ACK, so the caller must retransmit the 2xx.
func (d *Dialog) ReInviteNoAck(ctx context.Context, sdp []byte) (*sip.Response, error) {
	return d.reInvite(ctx, sdp, false, nil)
}

// ReInviteOfferless sends an offerless re-INVITE and ACKs the 2xx with ackSDP
// (the answer to the caller's offer).
func (d *Dialog) ReInviteOfferless(ctx context.Context, ackSDP []byte) (*sip.Response, error) {
	return d.reInvite(ctx, nil, true, ackSDP)
}

// Ack sends the ACK for the last re-INVITE sent with ReInviteNoAck,
// optionally with an SDP body.
func (d *Dialog) Ack(sdp []byte) error {
	inv := d.sess.InviteRequest
	contact := inv.Contact()
	if contact == nil {
		return errors.New("siptest: INVITE has no Contact")
	}
	ack := sip.NewRequest(sip.ACK, contact.Address)
	ack.SetTransport(inv.Transport())
	if len(sdp) > 0 {
		ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ack.SetBody(sdp)
	}
	return d.sess.WriteRequest(ack)
}

// FarEndRequest builds a request of method with this dialog's identifiers as
// the far end would send it to the caller (From = the trunk's side with its
// tag, To = the caller, Call-ID, the given CSeq and Max-Forwards). Via,
// Contact and body are the test's to add. The Request-URI is the caller's
// Contact.
func (d *Dialog) FarEndRequest(method string, cseq uint32) *sip.Request {
	inv := d.sess.InviteRequest
	req := sip.NewRequest(sip.RequestMethod(method), inv.Contact().Address)
	req.SetTransport(inv.Transport())
	from := d.sess.InviteResponse.To().AsFrom()
	req.AppendHeader(&from)
	to := inv.From().AsTo()
	req.AppendHeader(&to)
	req.AppendHeader(sip.HeaderClone(inv.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.RequestMethod(method)})
	maxFwd := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxFwd)
	return req
}

// ReInviteContact sends a re-INVITE whose Contact is contactURI (a target
// refresh, RFC 3261 12.2.2) and ACKs a 2xx.
func (d *Dialog) ReInviteContact(ctx context.Context, contactURI string, sdp []byte) (*sip.Response, error) {
	var u sip.Uri
	if err := sip.ParseUri(contactURI, &u); err != nil {
		return nil, err
	}
	inv := d.sess.InviteRequest
	contact := inv.Contact()
	if contact == nil {
		return nil, errors.New("siptest: INVITE has no Contact")
	}
	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.SetTransport(inv.Transport())
	req.AppendHeader(&sip.ContactHeader{Address: u})
	if sdp != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}
	res, err := d.sess.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.IsSuccess() {
		return res, d.Ack(nil)
	}
	return res, nil
}

// Trunk is the trunk this dialog belongs to.
func (d *Dialog) Trunk() *Trunk { return d.tr }

// ReInviteCSeq sends a re-INVITE with an explicit CSeq (sipgo would pick the
// next one) and ACKs a 2xx.
func (d *Dialog) ReInviteCSeq(ctx context.Context, cseq uint32, sdp []byte) (*sip.Response, error) {
	inv := d.sess.InviteRequest
	contact := inv.Contact()
	if contact == nil {
		return nil, errors.New("siptest: INVITE has no Contact")
	}
	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.SetTransport(inv.Transport())
	from := d.sess.InviteResponse.To().AsFrom()
	req.AppendHeader(&from)
	to := inv.From().AsTo()
	req.AppendHeader(&to)
	req.AppendHeader(sip.HeaderClone(inv.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.INVITE})
	maxFwd := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxFwd)
	c := d.tr.contact
	req.AppendHeader(&c)
	if sdp != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}
	if d.tr.ReuseConnection {
		req.SetDestination(inv.Source())
	}
	res, err := d.tr.client.Do(ctx, req, sipgo.ClientRequestAddVia)
	if err != nil {
		return nil, err
	}
	if res.IsSuccess() {
		ack := sip.NewRequest(sip.ACK, contact.Address)
		ack.SetTransport(inv.Transport())
		ack.AppendHeader(&from)
		ack.AppendHeader(&to)
		ack.AppendHeader(sip.HeaderClone(inv.CallID()))
		ack.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.ACK})
		ack.AppendHeader(&maxFwd)
		if d.tr.ReuseConnection {
			ack.SetDestination(inv.Source())
		}
		if err := d.tr.client.WriteRequest(ack, sipgo.ClientRequestAddVia); err != nil {
			return res, fmt.Errorf("siptest: send ACK: %w", err)
		}
	}
	return res, nil
}

func (d *Dialog) reInvite(ctx context.Context, sdp []byte, ack bool, ackSDP []byte) (*sip.Response, error) {
	inv := d.sess.InviteRequest
	contact := inv.Contact()
	if contact == nil {
		return nil, errors.New("siptest: INVITE has no Contact")
	}
	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.SetTransport(inv.Transport())
	if sdp != nil {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}
	res, err := d.sess.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.IsSuccess() && ack {
		ackReq := sip.NewRequest(sip.ACK, contact.Address)
		ackReq.SetTransport(inv.Transport())
		if len(ackSDP) > 0 {
			ackReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			ackReq.SetBody(ackSDP)
		}
		if err := d.sess.WriteRequest(ackReq); err != nil {
			return res, fmt.Errorf("siptest: send ACK: %w", err)
		}
	}
	return res, nil
}

// watchCancel hooks CANCEL for a pending INVITE. sipgo's transaction layer
// answers such a CANCEL itself (200 for the CANCEL, then 487 for the INVITE)
// and never calls a handler, so the hook is the only place the trunk sees it.
// The callback runs inside the transaction FSM: it only records and signals.
func (tr *Trunk) watchCancel(req *sip.Request, tx sip.ServerTransaction) {
	ch := make(chan struct{})
	id := ""
	if h := req.CallID(); h != nil {
		id = h.Value()
	}
	tr.mu.Lock()
	tr.cancels[id] = ch
	tr.mu.Unlock()
	tx.OnCancel(func(cancel *sip.Request) {
		tr.record(cancel)
		select {
		case <-ch:
		default:
			close(ch)
		}
	})
}

// CancelSeen returns a channel closed once a CANCEL for this INVITE has
// arrived (it is also in the request log as "CANCEL"). Use it in an INVITE
// handler to hold back the answer until the CANCEL crosses it:
//
//	<-tr.CancelSeen(req)
//	tr.RespondRaw(req, 200, "OK", "late-tag", sdp)
func (tr *Trunk) CancelSeen(req *sip.Request) <-chan struct{} {
	id := ""
	if h := req.CallID(); h != nil {
		id = h.Value()
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	ch, ok := tr.cancels[id]
	if !ok {
		ch = make(chan struct{}) // INVITE not routed yet; never closes
	}
	return ch
}

// WriteResponse writes res straight to the caller (its connection, or the
// UDP flow), bypassing the server transaction. Unlike tx.Respond it works
// after the transaction has answered 487 or 200, so a test can send a late
// 200 or several 2xx for one INVITE.
func (tr *Trunk) WriteResponse(res *sip.Response) error {
	return tr.ua.TransportLayer().WriteMsg(res)
}

// RespondRaw builds a response to req with toTag as the To tag (any code;
// empty toTag keeps a fresh generated one), adds our Contact to 2xx and a
// Content-Type for a body, and sends it with WriteResponse. Call it twice
// with different tags to simulate forked 2xx answers.
func (tr *Trunk) RespondRaw(req *sip.Request, code int, reason, toTag string, body []byte, headers ...sip.Header) error {
	res := sip.NewResponseFromRequest(req, code, reason, body)
	if toTag != "" {
		if to := res.To(); to != nil {
			to.Params.Add("tag", toTag)
		}
	}
	for _, h := range headers {
		res.AppendHeader(h)
	}
	if code >= 200 && code < 300 && res.Contact() == nil {
		c := tr.contact
		res.AppendHeader(&c)
	}
	if len(body) > 0 && res.GetHeader("Content-Type") == nil {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return tr.WriteResponse(res)
}

// CloseConnection closes the TCP/TLS connection req arrived on, the way a
// far end or a NAT drops a signaling connection mid-call. It reports whether
// a connection was found.
func (tr *Trunk) CloseConnection(req *sip.Request) bool {
	conn, err := tr.ua.TransportLayer().GetConnection(tr.transport, req.Source())
	if err != nil || conn == nil {
		return false
	}
	conn.Close()
	return true
}

// ByeUnknownDialog sends the caller a BYE whose To tag matches no dialog it
// has and returns the answer (a correct caller replies 481).
func (d *Dialog) ByeUnknownDialog(ctx context.Context) (*sip.Response, error) {
	inv := d.sess.InviteRequest
	contact := inv.Contact()
	if contact == nil {
		return nil, errors.New("siptest: INVITE has no Contact")
	}
	req := sip.NewRequest(sip.BYE, contact.Address)
	req.SetTransport(inv.Transport())
	to := inv.From().AsTo()
	to.Params.Add("tag", "bogus-tag")
	req.AppendHeader(&to)
	return d.sess.Do(ctx, req)
}
