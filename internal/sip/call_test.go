package sip

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

// branchOf returns the branch of the top Via value.
func branchOf(via string) string {
	_, rest, ok := strings.Cut(via, "branch=")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, ";, "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func TestCancelFollowsProxy(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				<-tr.CancelSeen(req)
			})
			// The target host does not exist; only the request's proxy is reachable.
			u := newTestUA(t, tr, func(o *Options) { o.NextHop = "127.0.0.1:9" })
			ringing := make(chan struct{})
			var once sync.Once
			a := start(t, u, dialRequest(tr, func(r *DialRequest) {
				r.Target = "sip:200@pbx.invalid"
				r.Transport = tp
				r.NextHop = tr.HostPort()
				r.OnResponse = func(status int, _ string) {
					if status == 180 {
						once.Do(func() { close(ringing) })
					}
				}
			}))
			select {
			case <-ringing:
			case <-time.After(5 * time.Second):
				t.Fatal("never saw 180")
			}
			a.Abandon()
			cancel, ok := tr.WaitRequest("CANCEL", 1, 3*time.Second)
			if !ok {
				t.Fatal("the CANCEL never reached the proxy")
			}
			inv := tr.RequestsFor("INVITE")[0]
			if branchOf(cancel.Header("Via")) != branchOf(inv.Header("Via")) || branchOf(inv.Header("Via")) == "" {
				t.Errorf("CANCEL Via %q does not match INVITE Via %q", cancel.Header("Via"), inv.Header("Via"))
			}
			if cancel.RequestURI != inv.RequestURI {
				t.Errorf("CANCEL Request-URI %q != INVITE %q", cancel.RequestURI, inv.RequestURI)
			}
			wantCSeq := strings.Fields(inv.Header("CSeq"))[0] + " CANCEL"
			if cancel.Header("CSeq") != wantCSeq || cancel.Header("Call-ID") != inv.Header("Call-ID") {
				t.Errorf("CANCEL CSeq/Call-ID = %q/%q, want %q/%q", cancel.Header("CSeq"), cancel.Header("Call-ID"), wantCSeq, inv.Header("Call-ID"))
			}
			closeBounded(t, u)
		})
	}
}

// stalledListener accepts TCP connections and never speaks.
func stalledListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().(*net.TCPAddr).Port
}

// farEndLookup builds the request that must find the UAC dialog of invite
// answered with toTag.
func farEndLookup(inv *gosip.Request, toTag string) *gosip.Request {
	req := gosip.NewRequest(gosip.BYE, gosip.Uri{Scheme: "sip", Host: "127.0.0.1"})
	from := inv.To().AsFrom()
	from.Params.Add("tag", toTag)
	req.AppendHeader(&from)
	to := inv.From().AsTo()
	req.AppendHeader(&to)
	req.AppendHeader(gosip.HeaderClone(inv.CallID()))
	req.AppendHeader(&gosip.CSeqHeader{SeqNo: 9, MethodName: gosip.BYE})
	return req
}

func TestAckToStalledContactDoesNotHang(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	port := stalledListener(t)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		contact := &gosip.ContactHeader{Address: gosip.Uri{
			Scheme: "sip", User: "node", Host: "127.0.0.1", Port: port,
			UriParams: gosip.HeaderParams{{K: "transport", V: "tls"}},
		}}
		tr.RespondRaw(req, 200, "OK", "stall1", answerBody(), contact)
	})
	u := newTestUA(t, tr, nil)
	u.ioTimeout = 400 * time.Millisecond
	a := start(t, u, dialRequest(tr, nil))
	r := result(t, a, 4*time.Second)
	if r.Call != nil || r.Cause != FailError || r.Reason != "ack_failed" {
		t.Fatalf("result = %+v, want sip_error/ack_failed", r)
	}
	inv, _ := tr.WaitRequest("INVITE", 1, time.Second)
	eventually(t, "dialog closed", func() bool {
		_, err := u.dialogs.MatchRequestDialog(farEndLookup(inv.Req, "stall1"))
		return err != nil
	})
	closeBounded(t, u)
}

func countTag(reqs []siptest.Request, tag string) int {
	n := 0
	for _, r := range reqs {
		if strings.Contains(r.Header("To"), "tag="+tag) {
			n++
		}
	}
	return n
}

func TestRetransmitted2xxAckedPerTag(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	reqs := make(chan *gosip.Request, 1)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) { reqs <- req })
	tr.OnBye(byeOK)
	u := newTestUA(t, tr, nil)
	a := start(t, u, dialRequest(tr, nil))
	req := <-reqs
	rr := func() gosip.Header {
		return gosip.NewHeader("Record-Route", fmt.Sprintf("<sip:127.0.0.1:%d;lr;transport=udp>", tr.Addr().Port()))
	}
	if err := tr.RespondRaw(req, 200, "OK", "tagA", answerBody(), rr()); err != nil {
		t.Fatal(err)
	}
	r := result(t, a, 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	if err := tr.RespondRaw(req, 200, "OK", "tagB", answerBody(), rr()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ACK and BYE for fork B", func() bool {
		return countTag(tr.RequestsFor("ACK"), "tagB") >= 1 && countTag(tr.RequestsFor("BYE"), "tagB") >= 1
	})
	tr.RespondRaw(req, 200, "OK", "tagA", answerBody(), rr()) // retransmission of A
	eventually(t, "second ACK for A", func() bool { return countTag(tr.RequestsFor("ACK"), "tagA") >= 2 })
	tr.RespondRaw(req, 200, "OK", "tagB", answerBody(), rr()) // retransmission of B after its BYE
	eventually(t, "second ACK for B", func() bool { return countTag(tr.RequestsFor("ACK"), "tagB") >= 2 })
	time.Sleep(300 * time.Millisecond)
	acks := tr.RequestsFor("ACK")
	if a, b := countTag(acks, "tagA"), countTag(acks, "tagB"); a != 2 || b != 2 {
		t.Errorf("ACKs: A=%d B=%d, want 2 each (one per 2xx of that tag)", a, b)
	}
	for _, ack := range acks {
		if n := len(ack.HeaderAll("Route")); n != 1 {
			t.Errorf("ACK has %d Route headers, want 1 (no accumulation): %v", n, ack.Headers)
		}
	}
	closeBounded(t, u)
}

func TestUDPCallKeepalive(t *testing.T) {
	tr, u, c, _ := establish(t, "udp", func(u *UA) { u.callKeepalive = 40 * time.Millisecond })
	base := tr.Keepalives()
	eventually(t, "CRLF pings during a UDP call", func() bool { return tr.Keepalives() >= base+3 })
	ctx := ctx5(t)
	if err := c.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	n := tr.Keepalives()
	time.Sleep(250 * time.Millisecond)
	if tr.Keepalives() != n {
		t.Error("pings continued after the call ended")
	}
	closeBounded(t, u)
}

func TestNextHopResolvedOnce(t *testing.T) {
	for _, tp := range []string{"udp", "tcp"} {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			newFakeRegistrar(tr, 0)
			tr.OnInvite(answer200(tr))
			tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) { byeOK(req, tx) })
			res := siptest.NewResolver(t, map[string]netip.Addr{"pbx.test": netip.MustParseAddr("127.0.0.1")})
			var dials atomic.Int32
			inner := res.Dial
			res.Dial = func(ctx context.Context, n, a string) (net.Conn, error) {
				dials.Add(1)
				return inner(ctx, n, a)
			}
			u := newTestUA(t, tr, func(o *Options) {
				o.NextHop = fmt.Sprintf("pbx.test:%d", tr.Addr().Port())
				o.Resolver = res
				o.Username, o.Password = testUser, testPass
			})
			u.regRetry = 100 * time.Millisecond
			// Resolve once up front (a UDP UA already did in NewUA).
			if _, err := u.ResolveNextHop(context.Background()); err != nil {
				t.Fatal(err)
			}
			n0 := dials.Load()

			u.DiscoverContact(ctx5(t), tr.URI("600"))
			_, log, _ := startReg(t, u, tr, 60*time.Second)
			waitState(t, log, "registered", registered)
			r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			kctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			u.StartKeepalive(kctx, 30*time.Millisecond, nil)
			base := tr.Keepalives()
			eventually(t, "keepalives", func() bool { return tr.Keepalives() >= base+2 })
			if n := dials.Load(); n != n0 {
				t.Errorf("next hop resolved %d more times; OPTIONS, REGISTER, INVITE and keepalive must share one resolution", n-n0)
			}
			closeBounded(t, u)
		})
	}
}

func TestReinviteTargetRefresh(t *testing.T) {
	tr, u, c, d := establish(t, "udp", nil)
	moved := fmt.Sprintf("sip:moved@127.0.0.1:%d;transport=udp", tr.Addr().Port())
	res, err := d.ReInviteContact(ctx5(t), moved, reofferBody(30000, ""))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	if err := c.Hangup(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	bye, ok := tr.WaitRequest("BYE", 1, 5*time.Second)
	if !ok {
		t.Fatal("no BYE")
	}
	if !strings.Contains(bye.RequestURI, "moved@") {
		t.Errorf("BYE Request-URI = %q, want the refreshed target", bye.RequestURI)
	}
	closeBounded(t, u)
}

func TestReinvite2xxRetransmitsOnReliableTransports(t *testing.T) {
	for _, tp := range []string{"tcp", "tls"} {
		t.Run(tp, func(t *testing.T) {
			_, u, _, d := establish(t, tp, func(u *UA) {
				u.reinviteT1, u.reinviteT2 = 40*time.Millisecond, 160*time.Millisecond
			})
			res, err := d.ReInviteNoAck(ctx5(t), reofferBody(30000, ""))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("re-INVITE = %v, %v", res, err)
			}
			cseq := res.CSeq().SeqNo
			eventually(t, "retransmitted 2xx over "+tp, func() bool { return countInviteOKs(d.Trunk(), cseq) >= 3 })
			if err := d.Ack(nil); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			n := countInviteOKs(d.Trunk(), cseq)
			time.Sleep(500 * time.Millisecond)
			if m := countInviteOKs(d.Trunk(), cseq); m != n {
				t.Errorf("2xx retransmitted after the ACK: %d -> %d", n, m)
			}
			closeBounded(t, u)
		})
	}
}

func TestStrictRouterRecordRoute(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		// A strict router (no lr) that is in fact the trunk itself.
		rr := gosip.NewHeader("Record-Route", fmt.Sprintf("<sip:127.0.0.1:%d;transport=udp>", tr.Addr().Port()))
		tr.RespondRaw(req, 200, "OK", "strict1", answerBody(), rr)
	})
	tr.OnBye(byeOK)
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	if err := r.Call.Hangup(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"ACK", "BYE"} {
		q, ok := tr.WaitRequest(m, 1, 5*time.Second)
		if !ok {
			t.Fatalf("no %s", m)
		}
		if strings.Contains(q.RequestURI, "trunk@") {
			t.Errorf("%s Request-URI = %q, want the strict router's URI", m, q.RequestURI)
		}
		routes := q.HeaderAll("Route")
		if len(routes) != 1 || !strings.Contains(routes[0], "trunk@") {
			t.Errorf("%s Route = %v, want only the remote target last", m, routes)
		}
	}
	closeBounded(t, u)
}

func TestSessionClosedWhenCallEnds(t *testing.T) {
	t.Run("hangup", func(t *testing.T) {
		_, u, c, d := establish(t, "udp", nil)
		req := d.FarEndRequest("BYE", 9)
		if _, err := u.dialogs.MatchRequestDialog(req); err != nil {
			t.Fatalf("dialog missing while the call is up: %v", err)
		}
		if err := c.Hangup(ctx5(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := u.dialogs.MatchRequestDialog(req); err == nil {
			t.Error("dialog still cached after Hangup")
		}
		closeBounded(t, u)
	})
	t.Run("remote bye", func(t *testing.T) {
		_, u, c, d := establish(t, "udp", nil)
		req := d.FarEndRequest("BYE", 9)
		if err := d.Bye(ctx5(t)); err != nil {
			t.Fatal(err)
		}
		<-c.Ended()
		eventually(t, "dialog closed", func() bool {
			_, err := u.dialogs.MatchRequestDialog(req)
			return err != nil
		})
		closeBounded(t, u)
	})
	t.Run("signaling lost", func(t *testing.T) {
		tr, u, c, d := establish(t, "udp", func(u *UA) {
			u.reinviteT1, u.reinviteT2, u.reinviteAckTimeout = 20*time.Millisecond, 40*time.Millisecond, 300*time.Millisecond
		})
		req := d.FarEndRequest("BYE", 9)
		if _, err := d.ReInviteNoAck(ctx5(t), reofferBody(30000, "")); err != nil {
			t.Fatal(err)
		}
		if cause := <-c.Ended(); cause != TransportLost {
			t.Fatalf("Ended = %q", cause)
		}
		_ = tr
		eventually(t, "dialog closed", func() bool {
			_, err := u.dialogs.MatchRequestDialog(req)
			return err != nil
		})
		closeBounded(t, u)
	})
}

// captureHandler records formatted log lines.
type captureHandler struct {
	mu    *sync.Mutex
	lines *[]string
	inner slog.Handler
}

func newCapture() (*captureHandler, func() string) {
	var mu sync.Mutex
	var lines []string
	h := &captureHandler{mu: &mu, lines: &lines}
	h.inner = slog.NewTextHandler(captureWriter{h}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return h, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

type captureWriter struct{ h *captureHandler }

func (w captureWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	*w.h.lines = append(*w.h.lines, string(bytes.TrimSpace(p)))
	w.h.mu.Unlock()
	return len(p), nil
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}
func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.inner.Handle(ctx, r)
}
func (h *captureHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &captureHandler{mu: h.mu, lines: h.lines, inner: h.inner.WithAttrs(a)}
}
func (h *captureHandler) WithGroup(n string) slog.Handler {
	return &captureHandler{mu: h.mu, lines: h.lines, inner: h.inner.WithGroup(n)}
}

const secretMsg = "INVITE sip:x@y SIP/2.0\r\nAuthorization: Digest username=\"u\", response=\"TOPSECRETRESP\"\r\n" +
	"Content-Length: zz\r\n\r\nv=0\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:TOPSECRETKEYTOPSECRETKEY\r\n"

func TestRedactingHandler(t *testing.T) {
	h, dump := newCapture()
	l := slog.New(newRedactHandler(h)).With("extra", "Proxy-Authorization: Digest response=\"TOPSECRETRESP\"").WithGroup("g")
	l.Error("failed to parse", "data", secretMsg, "error", fmt.Errorf("bad %s", secretMsg))
	out := dump()
	if strings.Contains(out, "TOPSECRET") {
		t.Errorf("secrets leaked: %s", out)
	}
	if !strings.Contains(out, "failed to parse") || !strings.Contains(out, "<redacted>") {
		t.Errorf("record lost or not redacted: %s", out)
	}
}

func TestParserErrorsAreRedacted(t *testing.T) {
	h, dump := newCapture()
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, func(o *Options) { o.Logger = slog.New(h) })
	src := learnFlow(t, tr, u)
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte(secretMsg), net.UDPAddrFromAddrPort(src)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the parse failure to be logged", func() bool { return strings.Contains(dump(), "failed to parse") })
	if out := dump(); strings.Contains(out, "TOPSECRET") {
		t.Errorf("parser error log leaked secrets: %s", out)
	}
}

func TestSipgoDefaultLoggerIsRedacted(t *testing.T) {
	h, dump := newCapture()
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)
	gosip.DefaultLogger().Error("failed to parse", "data", secretMsg)
	if out := dump(); strings.Contains(out, "TOPSECRET") || !strings.Contains(out, "failed to parse") {
		t.Errorf("default sipgo logger output = %s", out)
	}
}

func TestCloseDuringStalledAck(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	port := stalledListener(t)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		contact := &gosip.ContactHeader{Address: gosip.Uri{
			Scheme: "sip", User: "node", Host: "127.0.0.1", Port: port,
			UriParams: gosip.HeaderParams{{K: "transport", V: "tls"}},
		}}
		tr.RespondRaw(req, 200, "OK", "stall2", answerBody(), contact)
	})
	u := newTestUA(t, tr, nil) // the default 5 s I/O bound must not make Close wait
	start(t, u, dialRequest(tr, nil))
	if _, ok := tr.WaitRequest("INVITE", 1, 5*time.Second); !ok {
		t.Fatal("no INVITE")
	}
	time.Sleep(300 * time.Millisecond) // the ACK handshake is now stalled
	begin := time.Now()
	closeBounded(t, u)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("Close took %v with a stalled ACK handshake", d)
	}
}

func TestStrictRouterGetsTheRequest(t *testing.T) {
	router := siptest.NewTrunk(t, "udp")
	router.OnBye(byeOK)
	endpoint := siptest.NewTrunk(t, "udp")
	endpoint.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		rr := gosip.NewHeader("Record-Route", fmt.Sprintf("<sip:127.0.0.1:%d;transport=udp>", router.Addr().Port()))
		endpoint.RespondRaw(req, 200, "OK", "strict2", answerBody(), rr)
	})
	endpoint.OnBye(byeOK)
	u := newTestUA(t, endpoint, nil)
	r := result(t, start(t, u, dialRequest(endpoint, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	if err := r.Call.Hangup(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"ACK", "BYE"} {
		if _, ok := router.WaitRequest(m, 1, 5*time.Second); !ok {
			t.Errorf("the strict router never received the %s", m)
		}
		if n := len(endpoint.RequestsFor(m)); n != 0 {
			t.Errorf("the endpoint got the %s directly (%d), skipping the strict router", m, n)
		}
	}
	closeBounded(t, u)
}

func TestForkByeAfterFailedAck(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	reqs := make(chan *gosip.Request, 1)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) { reqs <- req })
	tr.OnBye(byeOK)
	// The fork's Contact: nothing listens there yet, so its first ACK fails.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	forkContact := func() gosip.Header {
		return &gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "fork", Host: "127.0.0.1", Port: port,
			UriParams: gosip.HeaderParams{{K: "transport", V: "tcp"}}}}
	}
	u := newTestUA(t, tr, nil)
	u.ioTimeout = 500 * time.Millisecond
	a := start(t, u, dialRequest(tr, nil))
	req := <-reqs
	if err := tr.RespondRaw(req, 200, "OK", "main", answerBody()); err != nil {
		t.Fatal(err)
	}
	if r := result(t, a, 5*time.Second); r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	tr.RespondRaw(req, 200, "OK", "fork", answerBody(), forkContact()) // its ACK is refused
	time.Sleep(300 * time.Millisecond)

	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("cannot reuse port %d: %v", port, err)
	}
	var mu sync.Mutex
	var got bytes.Buffer
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					mu.Lock()
					got.Write(buf[:n])
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	tr.RespondRaw(req, 200, "OK", "fork", answerBody(), forkContact()) // retransmitted 2xx
	eventually(t, "ACK then BYE to the fork", func() bool {
		mu.Lock()
		defer mu.Unlock()
		s := got.String()
		i := strings.Index(s, "ACK sip:")
		return i >= 0 && strings.Contains(s[i:], "BYE sip:")
	})
	closeBounded(t, u)
}

func TestCloseDuringStalledChallengeRetry(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	port := int(tr.Addr().Port())
	var once sync.Once
	var accepted atomic.Int32
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		siptest.Challenge(req, tx, false, testRealm, "retry-nonce", false)
		// The authenticated retry must open a new connection; make that one
		// stall in the TLS handshake.
		once.Do(func() {
			tr.CloseConnection(req)
			tr.StopListening()
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					accepted.Add(1)
					t.Cleanup(func() { c.Close() })
				}
			}()
		})
	})
	u := newTestUA(t, tr, withCredentials)
	start(t, u, dialRequest(tr, func(r *DialRequest) {
		// Runs before sipgo sends the authenticated retry: give the trunk time
		// to drop the first connection so the retry has to dial again.
		r.OnResponse = func(status int, _ string) {
			if status == 401 {
				time.Sleep(400 * time.Millisecond)
			}
		}
	}))
	if _, ok := tr.WaitRequest("INVITE", 1, 5*time.Second); !ok {
		t.Fatal("no INVITE")
	}
	eventually(t, "the retry to reach the stalled listener", func() bool { return accepted.Load() > 0 })
	time.Sleep(200 * time.Millisecond) // the retry is now inside its handshake
	begin := time.Now()
	closeBounded(t, u)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("Close took %v with a stalled retry handshake", d)
	}
}
