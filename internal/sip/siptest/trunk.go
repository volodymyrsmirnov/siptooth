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

	keepalives atomic.Int64

	mu       sync.Mutex
	requests []Request
	handlers map[string]Handler
	sni      []string
	dialogs  map[string]*Dialog
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
	}
	return data, nil
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
	if res.IsSuccess() {
		ack := sip.NewRequest(sip.ACK, contact.Address)
		ack.SetTransport(inv.Transport())
		if err := d.sess.WriteRequest(ack); err != nil {
			return res, fmt.Errorf("siptest: send ACK: %w", err)
		}
	}
	return res, nil
}
