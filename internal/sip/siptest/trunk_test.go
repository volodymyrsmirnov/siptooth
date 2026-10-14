package siptest_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

const offerSDP = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0\r\n"
const answerSDP = "v=0\r\no=- 2 2 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5000 RTP/AVP 0\r\n"
const reofferSDP = "v=0\r\no=- 3 3 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5002 RTP/AVP 0\r\n"

func freePort(t *testing.T, transport string) int {
	t.Helper()
	if transport == "udp" {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newUA(t *testing.T, tr *siptest.Trunk, localPort int) *sip.UA {
	t.Helper()
	o := sip.Options{Transport: tr.Transport(), NextHop: tr.HostPort(), LocalPort: localPort, UserAgent: "siptooth/test"}
	if tr.Transport() == "tls" {
		o.TLSConfig = tr.ClientTLSConfig()
	}
	u, err := sip.NewUA(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	return u
}

func TestTrunkRecordsRequestsAndScriptsOptions(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
		tx.Respond(gosip.NewResponseFromRequest(req, 418, "Teapot", nil))
	})
	u := newUA(t, tr, 0)
	req := gosip.NewRequest(gosip.OPTIONS, gosip.Uri{Scheme: "sip", User: "600", Host: "127.0.0.1", Port: int(tr.Addr().Port())})
	req.SetTransport("TCP")
	req.AppendHeader(gosip.NewHeader("X-Test", "one"))
	req.AppendHeader(gosip.NewHeader("X-Test", "two"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := u.Client().Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 418 {
		t.Errorf("status = %d, want the scripted 418", res.StatusCode)
	}
	got, ok := tr.WaitRequest("options", 1, time.Second)
	if !ok {
		t.Fatal("OPTIONS not recorded")
	}
	if got.Method != "OPTIONS" || !strings.HasPrefix(got.RequestURI, "sip:600@127.0.0.1") {
		t.Errorf("recorded %s %s", got.Method, got.RequestURI)
	}
	if v := got.HeaderAll("x-test"); len(v) != 2 || v[0] != "one" || v[1] != "two" {
		t.Errorf("X-Test = %v", v)
	}
	if got.Source == "" || got.Req == nil {
		t.Errorf("missing Source/Req: %+v", got)
	}
	if _, ok := tr.WaitRequest("OPTIONS", 2, 50*time.Millisecond); ok {
		t.Error("WaitRequest returned a request that was never sent")
	}
}

func TestTrunkDefaults(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newUA(t, tr, 0)
	do := func(m gosip.RequestMethod) *gosip.Response {
		req := gosip.NewRequest(m, gosip.Uri{Scheme: "sip", User: "x", Host: "127.0.0.1", Port: int(tr.Addr().Port())})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := u.Client().Do(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		return res
	}
	if res := do(gosip.OPTIONS); res.StatusCode != 200 || res.GetHeader("Allow") == nil {
		t.Errorf("default OPTIONS = %d, want 200 with Allow", res.StatusCode)
	}
	if res := do(gosip.REGISTER); res.StatusCode != 405 {
		t.Errorf("unscripted REGISTER = %d, want 405", res.StatusCode)
	}
	if res := do(gosip.BYE); res.StatusCode != 481 {
		t.Errorf("BYE outside a dialog = %d, want 481", res.StatusCode)
	}
	if n := len(tr.Requests()); n != 3 {
		t.Errorf("recorded %d requests, want 3", n)
	}
}

func TestRTPEchoReturnsDatagrams(t *testing.T) {
	addr := siptest.RTPEcho(t)
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := []byte{0x80, 0, 0, 1, 0, 0, 0, 160, 0, 0, 0, 1, 0xff, 0xff}
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != string(payload) {
		t.Errorf("echo = %x, %v", buf[:n], err)
	}
}

// TestDialogByeAndReInvite drives a full call: INVITE from the siptooth UA,
// the trunk answers, then the trunk sends a re-INVITE and a BYE back, to the
// Contact (UDP) and over the caller's own connection (TCP, TLS).
func TestDialogByeAndReInvite(t *testing.T) {
	cases := []struct {
		transport string
		reuse     bool
	}{{"udp", false}, {"udp", true}, {"tcp", true}, {"tls", true}}
	for _, c := range cases {
		name := c.transport
		if c.transport == "udp" && !c.reuse {
			name += "-contact"
		}
		t.Run(name, func(t *testing.T) {
			tr := siptest.NewTrunk(t, c.transport)
			tr.ReuseConnection = c.reuse
			dialogCh := make(chan *siptest.Dialog, 1)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				d, err := tr.Accept(req, tx, []byte(answerSDP))
				if err != nil {
					t.Errorf("Accept: %v", err)
					return
				}
				dialogCh <- d
			})

			lport := 0
			if c.transport == "udp" {
				lport = freePort(t, "udp") // the Contact must be reachable
			}
			u := newUA(t, tr, lport)

			reinvite := make(chan string, 1)
			bye := make(chan struct{}, 1)
			var sess *sipgo.DialogClientSession
			u.Server().OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				reinvite <- string(req.Body())
				res := gosip.NewSDPResponseFromRequest(req, []byte(offerSDP))
				res.AppendHeader(&gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "siptooth", Host: "127.0.0.1", Port: lport}})
				tx.Respond(res)
			})
			u.Server().OnAck(func(*gosip.Request, gosip.ServerTransaction) {})
			u.Server().OnBye(func(req *gosip.Request, tx gosip.ServerTransaction) {
				if err := sess.ReadBye(req, tx); err != nil {
					t.Errorf("ReadBye: %v", err)
				}
				bye <- struct{}{}
			})

			contactPort := lport
			if lport == 0 {
				contactPort = 9 // TCP/TLS: never dialled, the connection is reused
			}
			invite := gosip.NewRequest(gosip.INVITE, gosip.Uri{Scheme: "sip", User: "600", Host: "127.0.0.1", Port: int(tr.Addr().Port()),
				UriParams: gosip.HeaderParams{{K: "transport", V: c.transport}}})
			invite.SetTransport(strings.ToUpper(c.transport))
			invite.AppendHeader(&gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "siptooth", Host: "127.0.0.1", Port: contactPort,
				UriParams: gosip.HeaderParams{{K: "transport", V: c.transport}}}})
			invite.AppendHeader(gosip.NewHeader("Content-Type", "application/sdp"))
			invite.SetBody([]byte(offerSDP))

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			if sess, err = u.Dialogs().WriteInvite(ctx, invite); err != nil {
				t.Fatalf("INVITE: %v", err)
			}
			if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
				t.Fatalf("WaitAnswer: %v", err)
			}
			if body := string(sess.InviteResponse.Body()); body != answerSDP {
				t.Errorf("answer SDP = %q", body)
			}
			if err := sess.Ack(ctx); err != nil {
				t.Fatalf("Ack: %v", err)
			}

			var d *siptest.Dialog
			select {
			case d = <-dialogCh:
			case <-time.After(5 * time.Second):
				t.Fatal("trunk dialog never confirmed")
			}
			if d.State() < gosip.DialogStateConfirmed {
				t.Errorf("dialog state = %v, want confirmed", d.State())
			}
			if _, ok := tr.WaitRequest("ACK", 1, time.Second); !ok {
				t.Error("ACK not recorded")
			}

			res, err := d.ReInvite(ctx, []byte(reofferSDP))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("ReInvite = %v, %v", res, err)
			}
			if got := <-reinvite; got != reofferSDP {
				t.Errorf("re-INVITE body = %q", got)
			}
			if string(res.Body()) != offerSDP {
				t.Errorf("re-INVITE answer = %q", res.Body())
			}

			if err := d.Bye(ctx); err != nil {
				t.Fatalf("Bye: %v", err)
			}
			select {
			case <-bye:
			case <-time.After(5 * time.Second):
				t.Fatal("BYE did not reach the caller")
			}
		})
	}
}

func TestCallerByeIsAnsweredByDialog(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if _, err := tr.Accept(req, tx, []byte(answerSDP)); err != nil {
			t.Errorf("Accept: %v", err)
		}
	})
	u := newUA(t, tr, 0)
	invite := gosip.NewRequest(gosip.INVITE, gosip.Uri{Scheme: "sip", User: "600", Host: "127.0.0.1", Port: int(tr.Addr().Port())})
	invite.SetTransport("TCP")
	invite.AppendHeader(&gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "siptooth", Host: "127.0.0.1", Port: 9}})
	invite.AppendHeader(gosip.NewHeader("Content-Type", "application/sdp"))
	invite.SetBody([]byte(offerSDP))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := u.Dialogs().WriteInvite(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("caller Bye: %v", err)
	}
	if _, ok := tr.WaitRequest("BYE", 1, time.Second); !ok {
		t.Error("BYE not recorded")
	}
}

// capture records every response the client UA receives, at the transport
// layer (before transaction matching), so tests see exactly what reached it.
type capture struct {
	mu   chan struct{}
	resp []*gosip.Response
}

func watchResponses(u *sip.UA) *capture {
	c := &capture{mu: make(chan struct{}, 1)}
	u.UserAgent().TransportLayer().OnMessage(func(m gosip.Message) {
		if r, ok := m.(*gosip.Response); ok {
			c.mu <- struct{}{}
			c.resp = append(c.resp, r)
			<-c.mu
		}
	})
	return c
}

func (c *capture) snapshot() []*gosip.Response {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	return append([]*gosip.Response(nil), c.resp...)
}

func (c *capture) waitFor(t *testing.T, what string, cond func([]*gosip.Response) bool) []*gosip.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rs := c.snapshot(); cond(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; saw %d responses", what, len(c.snapshot()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sendInvite(t *testing.T, u *sip.UA, tr *siptest.Trunk) (*sipgo.DialogClientSession, context.Context, context.CancelFunc) {
	t.Helper()
	invite := gosip.NewRequest(gosip.INVITE, gosip.Uri{Scheme: "sip", User: "600", Host: "127.0.0.1", Port: int(tr.Addr().Port())})
	invite.SetTransport(strings.ToUpper(tr.Transport()))
	invite.AppendHeader(&gosip.ContactHeader{Address: gosip.Uri{Scheme: "sip", User: "siptooth", Host: "127.0.0.1", Port: 9}})
	invite.AppendHeader(gosip.NewHeader("Content-Type", "application/sdp"))
	invite.SetBody([]byte(offerSDP))
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := u.Dialogs().WriteInvite(ctx, invite)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return sess, ctx, cancel
}

func TestCancelIsRecordedAndCrossingAnswerReachesClient(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			answered := make(chan error, 1)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				<-tr.CancelSeen(req) // hold the answer until the CANCEL arrives
				answered <- tr.RespondRaw(req, 200, "OK", "late-tag", []byte(answerSDP))
			})
			lport := 0
			if transport == "udp" {
				lport = freePort(t, "udp")
			}
			u := newUA(t, tr, lport)
			seen := watchResponses(u)

			sess, ctx, cancel := sendInvite(t, u, tr)
			defer cancel()
			seen.waitFor(t, "180", func(rs []*gosip.Response) bool {
				for _, r := range rs {
					if r.StatusCode == 180 {
						return true
					}
				}
				return false
			})
			go func() { sess.WaitAnswer(ctx, sipgo.AnswerOptions{}) }()
			cancel() // sipgo sends CANCEL

			if _, ok := tr.WaitRequest("CANCEL", 1, 5*time.Second); !ok {
				t.Fatal("CANCEL never recorded: sipgo answers it inside the transaction layer")
			}
			select {
			case err := <-answered:
				if err != nil {
					t.Fatalf("RespondRaw: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("handler never saw the CANCEL")
			}
			seen.waitFor(t, "the crossing 200", func(rs []*gosip.Response) bool {
				for _, r := range rs {
					if r.StatusCode == 200 && r.CSeq() != nil && r.CSeq().MethodName == gosip.INVITE {
						if to := r.To(); to != nil {
							tag, _ := to.Params.Get("tag")
							return tag == "late-tag"
						}
					}
				}
				return false
			})
		})
	}
}

func TestTwoForkedAnswersReachClient(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				if err := tr.RespondRaw(req, 200, "OK", "fork-a", []byte(answerSDP)); err != nil {
					t.Errorf("fork a: %v", err)
				}
				if err := tr.RespondRaw(req, 200, "OK", "fork-b", []byte(answerSDP)); err != nil {
					t.Errorf("fork b: %v", err)
				}
			})
			lport := 0
			if transport == "udp" {
				lport = freePort(t, "udp")
			}
			u := newUA(t, tr, lport)
			seen := watchResponses(u)
			_, _, cancel := sendInvite(t, u, tr)
			defer cancel()

			rs := seen.waitFor(t, "two 200s", func(rs []*gosip.Response) bool {
				tags := map[string]bool{}
				for _, r := range rs {
					if r.StatusCode == 200 {
						tag, _ := r.To().Params.Get("tag")
						tags[tag] = true
					}
				}
				return tags["fork-a"] && tags["fork-b"]
			})
			for _, r := range rs {
				if r.StatusCode == 200 && r.Contact() == nil {
					t.Error("2xx without Contact")
				}
			}
		})
	}
}
