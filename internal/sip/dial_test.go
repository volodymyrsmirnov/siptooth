package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

var allTransports = []string{"udp", "tcp", "tls"}

const (
	testRealm = "siptooth.test"
	testUser  = "alice"
	testPass  = "s3cret"
)

func testOffer() sdp.Offer {
	return sdp.Offer{
		SessID: 1700000000, Version: 1,
		Addr:   netip.MustParseAddrPort("127.0.0.1:20000"),
		Codecs: []g711.Codec{g711.PCMU, g711.PCMA},
	}
}

// answerBody is an acceptable plain RTP answer pointing at 127.0.0.1:30000.
func answerBody() []byte {
	return []byte(strings.Join([]string{
		"v=0", "o=pbx 1 1 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
		"m=audio 30000 RTP/AVP 0", "a=rtpmap:0 PCMU/8000", "",
	}, "\r\n"))
}

func dialRequest(tr *siptest.Trunk, mod func(*DialRequest)) DialRequest {
	r := DialRequest{
		Target:      tr.URI("200"),
		From:        "sip:siptooth@example.test",
		DisplayName: `"Caller"`,
		Contact:     fmt.Sprintf("sip:siptooth@127.0.0.1:5090;transport=%s", tr.Transport()),
		Offer:       testOffer(),
	}
	if mod != nil {
		mod(&r)
	}
	return r
}

func start(t *testing.T, u *UA, req DialRequest) Attempt {
	t.Helper()
	a, err := u.Start(req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return a
}

func result(t *testing.T, a Attempt, within time.Duration) DialResult {
	t.Helper()
	select {
	case r := <-a.Result():
		return r
	case <-time.After(within):
		t.Fatalf("no DialResult within %v", within)
		return DialResult{}
	}
}

// noSecondResult asserts the channel stays empty: exactly one value per attempt.
func noSecondResult(t *testing.T, a Attempt) {
	t.Helper()
	select {
	case r := <-a.Result():
		t.Errorf("second DialResult: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func answer200(tr *siptest.Trunk) siptest.Handler {
	return func(req *gosip.Request, tx gosip.ServerTransaction) {
		tr.Accept(req, tx, answerBody())
	}
}

func byeOK(req *gosip.Request, tx gosip.ServerTransaction) {
	tx.Respond(gosip.NewResponseFromRequest(req, 200, "OK", nil))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closeBounded asserts UA.Close returns, which means every cleanup goroutine
// the UA started has ended.
func closeBounded(t *testing.T, u *UA) {
	t.Helper()
	done := make(chan struct{})
	go func() { u.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("UA.Close did not return: a background goroutine leaked")
	}
}

func TestDial200(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(answer200(tr))
			u := newTestUA(t, tr, nil)

			a := start(t, u, dialRequest(tr, nil))
			r := result(t, a, 5*time.Second)
			if r.Call == nil || r.Cause != "" {
				t.Fatalf("result = %+v, want a Call", r)
			}
			if _, ok := tr.WaitRequest("ACK", 1, 5*time.Second); !ok {
				t.Fatal("trunk never saw the ACK")
			}
			m := r.Call.Remote()
			if want := netip.MustParseAddrPort("127.0.0.1:30000"); m.Addr != want || m.SendCodec != g711.PCMU {
				t.Errorf("Remote = %+v", m)
			}
			if r.Call.SIPCallID() == "" {
				t.Error("empty Call-ID")
			}
			if s, reason := r.Call.LastStatus(); s != 200 || reason == "" {
				t.Errorf("LastStatus = %d %q", s, reason)
			}
			noSecondResult(t, a)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Call.Hangup(ctx); err != nil {
				t.Fatalf("Hangup: %v", err)
			}
			if _, ok := tr.WaitRequest("BYE", 1, 5*time.Second); !ok {
				t.Error("trunk never saw the BYE")
			}
			if err := r.Call.Hangup(ctx); err != nil {
				t.Errorf("second Hangup: %v", err)
			}
			closeBounded(t, u)
		})
	}
}

func TestDialRejected486(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				// sipgo handles each datagram on its own goroutine, so a
				// back-to-back 486 can overtake the 180.
				time.Sleep(100 * time.Millisecond)
				tx.Respond(gosip.NewResponseFromRequest(req, 486, "Busy Here", nil))
			})
			u := newTestUA(t, tr, nil)
			var seen []int
			var mu sync.Mutex
			a := start(t, u, dialRequest(tr, func(r *DialRequest) {
				r.OnResponse = func(status int, _ string) {
					mu.Lock()
					seen = append(seen, status)
					mu.Unlock()
				}
			}))
			r := result(t, a, 5*time.Second)
			if r.Cause != FailRejected || r.Status != 486 || r.Reason != "Busy Here" || r.Call != nil {
				t.Fatalf("result = %+v", r)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) < 2 || seen[0] != 180 || seen[len(seen)-1] != 486 {
				t.Errorf("OnResponse saw %v, want 180 ... 486", seen)
			}
		})
	}
}

func TestDial503(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		tx.Respond(gosip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
	})
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Cause != FailRejected || r.Status != 503 {
		t.Fatalf("result = %+v", r)
	}
}

func TestDialRedirectIsRejected(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		res := gosip.NewResponseFromRequest(req, 302, "Moved Temporarily", nil)
		res.AppendHeader(gosip.NewHeader("Contact", "<sip:elsewhere@127.0.0.1:1>"))
		tx.Respond(res)
	})
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Cause != FailRejected || r.Status != 302 {
		t.Fatalf("result = %+v", r)
	}
	if n := len(tr.RequestsFor("INVITE")); n != 1 {
		t.Errorf("redirect was followed: %d INVITEs", n)
	}
}

func TestDialConnectionRefusedIsSIPError(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	u := newTestUA(t, tr, nil)
	a := start(t, u, dialRequest(tr, func(d *DialRequest) { d.NextHop = "127.0.0.1:1" }))
	r := result(t, a, 10*time.Second)
	if r.Cause != FailError || r.Err == nil || r.Call != nil {
		t.Fatalf("result = %+v", r)
	}
}

// digestInvite challenges the first len(nonces) INVITEs, one nonce each (the
// later challenges are stale ones), checking that every retry answers the
// previous nonce; the next INVITE must answer the last nonce and is accepted.
func digestInvite(tr *siptest.Trunk, proxy bool, nonces []string, invites *atomic.Int32) siptest.Handler {
	return func(req *gosip.Request, tx gosip.ServerTransaction) {
		n := int(invites.Add(1)) // 1-based
		if n > 1 && !siptest.CheckDigest(req, testUser, testPass, nonces[min(n, len(nonces)+1)-2]) {
			tx.Respond(gosip.NewResponseFromRequest(req, 403, "Forbidden", nil))
			return
		}
		if n <= len(nonces) {
			siptest.Challenge(req, tx, proxy, testRealm, nonces[n-1], n > 1)
			return
		}
		tr.Accept(req, tx, answerBody())
	}
}

func withCredentials(o *Options) { o.Username, o.Password = testUser, testPass }

func TestDialDigest401(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			var invites atomic.Int32
			tr.OnInvite(digestInvite(tr, false, []string{"nonce-1"}, &invites))
			u := newTestUA(t, tr, withCredentials)
			r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			rs := tr.RequestsFor("INVITE")
			if len(rs) != 2 {
				t.Fatalf("%d INVITEs, want 2", len(rs))
			}
			if rs[0].Header("Call-ID") != rs[1].Header("Call-ID") {
				t.Error("retry changed the Call-ID")
			}
			if rs[0].Header("Authorization") != "" {
				t.Error("first INVITE carried credentials")
			}
			if !strings.HasPrefix(rs[1].Header("Authorization"), "Digest ") {
				t.Errorf("retry Authorization = %q", rs[1].Header("Authorization"))
			}
			if _, ok := tr.WaitRequest("ACK", 1, 5*time.Second); !ok {
				t.Error("no ACK")
			}
		})
	}
}

func TestDialDigest407(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	var invites atomic.Int32
	tr.OnInvite(digestInvite(tr, true, []string{"nonce-p"}, &invites))
	u := newTestUA(t, tr, withCredentials)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	rs := tr.RequestsFor("INVITE")
	if len(rs) != 2 || !strings.HasPrefix(rs[1].Header("Proxy-Authorization"), "Digest ") {
		t.Fatalf("INVITEs = %d, Proxy-Authorization = %q", len(rs), rs[len(rs)-1].Header("Proxy-Authorization"))
	}
}

func TestDialStaleNonceSecondChallenge(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	var invites atomic.Int32
	tr.OnInvite(digestInvite(tr, false, []string{"nonce-1", "nonce-2"}, &invites))
	u := newTestUA(t, tr, withCredentials)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	if n := invites.Load(); n != 3 {
		t.Errorf("%d INVITEs, want 3", n)
	}
}

func TestDialThirdChallengeFails(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	var invites atomic.Int32
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		n := invites.Add(1)
		siptest.Challenge(req, tx, false, testRealm, fmt.Sprintf("nonce-%d", n), n > 1)
	})
	u := newTestUA(t, tr, withCredentials)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Cause != FailError || r.Reason != "auth_failed" || r.Status != 401 || r.Call != nil {
		t.Fatalf("result = %+v", r)
	}
	if n := invites.Load(); n != 3 {
		t.Errorf("%d INVITEs, want 3 (initial plus two authenticated)", n)
	}
}

func TestDialNoUsername401Fails(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	var invites atomic.Int32
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		invites.Add(1)
		siptest.Challenge(req, tx, false, testRealm, "n", false)
	})
	// A password without a username must not be used either.
	u := newTestUA(t, tr, func(o *Options) { o.Password = testPass })
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Cause != FailError || r.Reason != "auth_required" || r.Status != 401 {
		t.Fatalf("result = %+v", r)
	}
	if n := invites.Load(); n != 1 {
		t.Errorf("%d INVITEs, want 1", n)
	}
}

func TestDialAbandonAfter180(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				<-tr.CancelSeen(req)
			})
			u := newTestUA(t, tr, nil)
			ringing := make(chan struct{})
			var once sync.Once
			a := start(t, u, dialRequest(tr, func(r *DialRequest) {
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
			begin := time.Now()
			a.Abandon()
			r := result(t, a, time.Second)
			if r.Cause != FailAbandoned || r.Call != nil {
				t.Fatalf("result = %+v", r)
			}
			if d := time.Since(begin); d > 50*time.Millisecond {
				t.Errorf("Abandon + Result took %v", d)
			}
			if _, ok := tr.WaitRequest("CANCEL", 1, 5*time.Second); !ok {
				t.Fatal("trunk never saw the CANCEL")
			}
			a.Abandon() // idempotent
			noSecondResult(t, a)
			closeBounded(t, u)
		})
	}
}

func TestDialAbandonBefore1xx(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			const delay = 120 * time.Millisecond // below sipgo's own 100 Trying at 200 ms
			var sent180 atomic.Int64
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				time.Sleep(delay)
				sent180.Store(time.Now().UnixNano())
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				<-tr.CancelSeen(req)
			})
			u := newTestUA(t, tr, nil)
			a := start(t, u, dialRequest(tr, nil))
			if _, ok := tr.WaitRequest("INVITE", 1, 5*time.Second); !ok {
				t.Fatal("no INVITE")
			}
			a.Abandon()
			if r := result(t, a, 50*time.Millisecond); r.Cause != FailAbandoned {
				t.Fatalf("result = %+v", r)
			}
			time.Sleep(delay / 2)
			if n := len(tr.RequestsFor("CANCEL")); n != 0 {
				t.Fatalf("CANCEL sent before any 1xx (RFC 3261 9.1)")
			}
			cancelReq, ok := tr.WaitRequest("CANCEL", 1, 5*time.Second)
			if !ok {
				t.Fatal("CANCEL never sent after the 180")
			}
			_ = cancelReq
			if sent180.Load() == 0 {
				t.Error("CANCEL arrived before the 180 was sent")
			}
			closeBounded(t, u)
		})
	}
}

func TestDialAbandonWhileConnecting(t *testing.T) {
	// A TLS server that accepts and never speaks: the INVITE write blocks in
	// the handshake until the 5 s write timeout (shortened here). Abandon
	// publishes at once; the goroutine ends on its own and sends no CANCEL.
	old := tcpWriteTimeout
	tcpWriteTimeout = 500 * time.Millisecond
	t.Cleanup(func() { tcpWriteTimeout = old })

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var cmu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cmu.Lock()
			conns = append(conns, c)
			cmu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		cmu.Lock()
		for _, c := range conns {
			c.Close()
		}
		cmu.Unlock()
	})

	tr := siptest.NewTrunk(t, "tls")
	u := newTestUA(t, tr, nil)
	a := start(t, u, dialRequest(tr, func(r *DialRequest) { r.NextHop = ln.Addr().String() }))
	time.Sleep(50 * time.Millisecond) // let the goroutine reach the handshake
	a.Abandon()
	if r := result(t, a, 50*time.Millisecond); r.Cause != FailAbandoned {
		t.Fatalf("result = %+v", r)
	}
	closeBounded(t, u)
	noSecondResult(t, a)
}

func TestDialCancelCrossing200(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnBye(byeOK)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tx.Respond(gosip.NewResponseFromRequest(req, 180, "Ringing", nil))
				<-tr.CancelSeen(req)
				tr.RespondRaw(req, 200, "OK", "late-tag", answerBody())
			})
			u := newTestUA(t, tr, nil)
			ringing := make(chan struct{})
			var once sync.Once
			a := start(t, u, dialRequest(tr, func(r *DialRequest) {
				r.OnResponse = func(status int, _ string) {
					if status == 180 {
						once.Do(func() { close(ringing) })
					}
				}
			}))
			<-ringing
			a.Abandon()
			if r := result(t, a, 50*time.Millisecond); r.Cause != FailAbandoned {
				t.Fatalf("result = %+v", r)
			}
			if _, ok := tr.WaitRequest("BYE", 1, 10*time.Second); !ok {
				t.Fatal("trunk never saw the BYE for the crossing 200")
			}
			requireAckThenBye(t, tr, "late-tag")
			noSecondResult(t, a)
			closeBounded(t, u)
		})
	}
}

// requireAckThenBye asserts the trunk received an ACK and a BYE for the same
// dialog (To tag containing tag, when not empty) and that the BYE's CSeq is
// above the ACK's. Arrival order in the trunk's log is not asserted: sipgo
// serves every message on its own goroutine, so it can swap two requests
// that were written in order on one connection.
func requireAckThenBye(t *testing.T, tr *siptest.Trunk, tag string) {
	t.Helper()
	bye, ok := tr.WaitRequest("BYE", 1, 10*time.Second)
	if !ok {
		t.Fatal("trunk never saw the BYE")
	}
	var ack *siptest.Request
	eventually(t, "the ACK", func() bool {
		for _, q := range tr.RequestsFor("ACK") {
			if strings.Contains(q.Header("To"), tag) {
				q := q
				ack = &q
				return true
			}
		}
		return false
	})
	if !strings.Contains(bye.Header("To"), tag) {
		t.Errorf("BYE To = %q, want tag %q", bye.Header("To"), tag)
	}
	seq := func(r siptest.Request) int {
		var n int
		fmt.Sscanf(r.Header("CSeq"), "%d", &n)
		return n
	}
	if seq(bye) <= seq(*ack) {
		t.Errorf("BYE CSeq %q not above ACK CSeq %q", bye.Header("CSeq"), ack.Header("CSeq"))
	}
}

func TestDialForkedSecond200(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnBye(byeOK)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tr.RespondRaw(req, 200, "OK", "fork-a", answerBody())
				tr.RespondRaw(req, 200, "OK", "fork-b", answerBody())
			})
			u := newTestUA(t, tr, nil)
			a := start(t, u, dialRequest(tr, nil))
			r := result(t, a, 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			kept := toTag(r.Call.(*call).sess.InviteResponse)
			other := "fork-a"
			if kept == "fork-a" {
				other = "fork-b"
			}
			bye, ok := tr.WaitRequest("BYE", 1, 10*time.Second)
			if !ok {
				t.Fatal("the second dialog never got a BYE")
			}
			if to := bye.Header("To"); !strings.Contains(to, other) || strings.Contains(to, kept) {
				t.Errorf("BYE To = %q, want tag %s (kept %s)", to, other, kept)
			}
			rs := tr.Requests()
			var acks []string
			for _, q := range rs {
				if q.Method == "ACK" {
					acks = append(acks, q.Header("To"))
				}
			}
			eventually(t, "two ACKs", func() bool {
				acks = nil
				for _, q := range tr.RequestsFor("ACK") {
					acks = append(acks, q.Header("To"))
				}
				return len(acks) >= 2
			})
			var gotKept, gotOther bool
			for _, to := range acks {
				gotKept = gotKept || strings.Contains(to, kept)
				gotOther = gotOther || strings.Contains(to, other)
			}
			if !gotKept || !gotOther {
				t.Errorf("ACK To headers %v, want both tags", acks)
			}
			if n := len(tr.RequestsFor("BYE")); n != 1 {
				t.Errorf("%d BYEs, want exactly one (the kept call stays up)", n)
			}
			noSecondResult(t, a)
			closeBounded(t, u)
		})
	}
}

func TestDialBadSDPAckBye(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				tr.Accept(req, tx, []byte("v=0\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 30000 RTP/AVP 18\r\n"))
			})
			u := newTestUA(t, tr, nil)
			a := start(t, u, dialRequest(tr, nil))
			r := result(t, a, 5*time.Second)
			if r.Cause != FailMedia || r.Status != 200 || r.Call != nil || !errors.Is(r.Err, sdp.ErrUnacceptable) {
				t.Fatalf("result = %+v", r)
			}
			if _, ok := tr.WaitRequest("BYE", 1, 10*time.Second); !ok {
				t.Fatal("trunk never saw the BYE")
			}
			requireAckThenBye(t, tr, "")
			noSecondResult(t, a)
			closeBounded(t, u)
		})
	}
}

func TestDialHeaders(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(answer200(tr))
			u := newTestUA(t, tr, nil)

			vals := placeholder.Values{Caller: `Al "Bob" \x`, Number: "#123"}
			req := dialRequest(tr, func(r *DialRequest) {
				r.DisplayName = placeholder.ExpandDisplayName("{caller}", vals)
				r.Target = fmt.Sprintf("sip:200%%23123@%s;transport=%s", tr.HostPort(), tp)
				r.Headers = map[string]string{"X-CALLER-ID": "+15551234"}
				r.From = "sip:+15551234@example.test"
			})
			r := result(t, start(t, u, req), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			inv, ok := tr.WaitRequest("INVITE", 1, time.Second)
			if !ok {
				t.Fatal("no INVITE")
			}
			from := inv.Header("From")
			if !strings.HasPrefix(from, `"Al \"Bob\" \\x" <sip:+15551234@example.test>;tag=`) {
				t.Errorf("From = %s", from)
			}
			if strings.Count(from, `"`) != 4 { // outer pair plus two escaped inner quotes
				t.Errorf("From quoting: %s", from)
			}
			if !strings.Contains(inv.RequestURI, "200%23123") {
				t.Errorf("Request-URI = %q, want the escaped %%23 kept", inv.RequestURI)
			}
			if got := inv.Header("X-CALLER-ID"); got != "+15551234" {
				t.Errorf("X-CALLER-ID = %q", got)
			}
			if got := inv.Header("Contact"); !strings.Contains(got, "sip:siptooth@127.0.0.1:5090") || !strings.Contains(got, "transport="+tp) {
				t.Errorf("Contact = %q", got)
			}
			if got := inv.Header("Allow"); got != "INVITE, ACK, CANCEL, BYE, OPTIONS" {
				t.Errorf("Allow = %q", got)
			}
			if got := inv.Header("User-Agent"); got != "siptooth/test" {
				t.Errorf("User-Agent = %q", got)
			}
			if got := inv.Header("Content-Type"); got != "application/sdp" {
				t.Errorf("Content-Type = %q", got)
			}
			if string(inv.Body) != string(testOffer().Marshal()) {
				t.Errorf("body = %q", inv.Body)
			}
			if inv.Header("Supported") != "" || inv.Header("Require") != "" {
				t.Error("INVITE advertises Supported/Require")
			}
			if to := inv.Header("To"); strings.Contains(to, "transport") {
				t.Errorf("To carries URI parameters: %s", to)
			}
		})
	}
}

func TestDialEmptyDisplayName(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		tx.Respond(gosip.NewResponseFromRequest(req, 486, "Busy Here", nil))
	})
	u := newTestUA(t, tr, nil)
	result(t, start(t, u, dialRequest(tr, func(r *DialRequest) { r.DisplayName = `""` })), 5*time.Second)
	inv, _ := tr.WaitRequest("INVITE", 1, time.Second)
	if from := inv.Header("From"); !strings.HasPrefix(from, "<sip:siptooth@example.test>") {
		t.Errorf("From = %q, want no display name", from)
	}
}

func TestStartValidatesRequest(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	u := newTestUA(t, tr, nil)
	for name, mod := range map[string]func(*DialRequest){
		"bad target":    func(r *DialRequest) { r.Target = "not a uri" },
		"no contact":    func(r *DialRequest) { r.Contact = "" },
		"bad from":      func(r *DialRequest) { r.From = "" },
		"bad transport": func(r *DialRequest) { r.Transport = "sctp" },
	} {
		if _, err := u.Start(dialRequest(tr, mod)); err == nil {
			t.Errorf("%s: Start succeeded", name)
		}
	}
	u.Close()
	if _, err := u.Start(dialRequest(tr, nil)); err == nil {
		t.Error("Start on a closed UA succeeded")
	}
}

func TestDialTLSWithoutTransportParam(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	tr.OnInvite(answer200(tr))
	u := newTestUA(t, tr, nil)
	req := dialRequest(tr, func(r *DialRequest) {
		r.Target = "sip:200@localhost"
		r.Transport = "tls"
		r.NextHop = tr.HostPort()
	})
	r := result(t, start(t, u, req), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	inv, _ := tr.WaitRequest("INVITE", 1, time.Second)
	if inv.RequestURI != "sip:200@localhost" {
		t.Errorf("Request-URI = %q", inv.RequestURI)
	}
	if !strings.Contains(inv.Header("Via"), "TLS") {
		t.Errorf("Via = %q", inv.Header("Via"))
	}
}

func TestDialViaProxy(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(answer200(tr))
			// The UA's configured next hop is unreachable: the request's own
			// proxy wins, and the target host is never resolved.
			u := newTestUA(t, tr, func(o *Options) { o.NextHop = "127.0.0.1:9" })
			req := dialRequest(tr, func(r *DialRequest) {
				r.Target = "sip:200@pbx.invalid"
				r.Transport = tp
				r.NextHop = tr.HostPort()
			})
			r := result(t, start(t, u, req), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			inv, _ := tr.WaitRequest("INVITE", 1, time.Second)
			if !strings.Contains(inv.RequestURI, "pbx.invalid") {
				t.Errorf("Request-URI = %q", inv.RequestURI)
			}
		})
	}
}

func TestHangupBYEChallenged(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			tr.OnInvite(answer200(tr))
			var byes atomic.Int32
			tr.OnBye(func(req *gosip.Request, tx gosip.ServerTransaction) {
				if byes.Add(1) == 1 {
					siptest.Challenge(req, tx, false, testRealm, "bye-nonce", false)
					return
				}
				if !siptest.CheckDigest(req, testUser, testPass, "bye-nonce") {
					tx.Respond(gosip.NewResponseFromRequest(req, 403, "Forbidden", nil))
					return
				}
				tx.Respond(gosip.NewResponseFromRequest(req, 200, "OK", nil))
			})
			u := newTestUA(t, tr, withCredentials)
			r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Call.Hangup(ctx); err != nil {
				t.Fatalf("Hangup: %v", err)
			}
			rs := tr.RequestsFor("BYE")
			if len(rs) != 2 {
				t.Fatalf("%d BYEs, want 2", len(rs))
			}
			if rs[0].Header("Authorization") != "" || rs[1].Header("Authorization") == "" {
				t.Errorf("Authorization on BYEs: %q, %q", rs[0].Header("Authorization"), rs[1].Header("Authorization"))
			}
			if rs[0].Header("CSeq") == rs[1].Header("CSeq") {
				t.Error("the authenticated BYE reused the CSeq")
			}
			select {
			case c := <-r.Call.Ended():
				t.Errorf("local Hangup reported Ended(%s)", c)
			default:
			}
		})
	}
}

func TestHangupBYEChallengedWithoutCredentialsFails(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnInvite(answer200(tr))
	tr.OnBye(func(req *gosip.Request, tx gosip.ServerTransaction) {
		siptest.Challenge(req, tx, false, testRealm, "n", false)
	})
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Call.Hangup(ctx); err == nil {
		t.Error("Hangup succeeded against a challenge without credentials")
	}
	if n := len(tr.RequestsFor("BYE")); n != 1 {
		t.Errorf("%d BYEs, want 1", n)
	}
}

func TestRemoteBye(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			dialogs := make(chan *siptest.Dialog, 1)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				d, err := tr.Accept(req, tx, answerBody())
				if err == nil {
					dialogs <- d
				}
			})
			u := newTestUA(t, tr, nil)
			r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			d := <-dialogs
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.Bye(ctx); err != nil {
				t.Fatalf("trunk BYE: %v", err)
			}
			select {
			case cause := <-r.Call.Ended():
				if cause != RemoteBye {
					t.Errorf("Ended = %q", cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Ended never fired")
			}
			if err := r.Call.Hangup(ctx); err != nil {
				t.Errorf("Hangup after remote BYE: %v", err)
			}
			if n := len(tr.RequestsFor("BYE")); n != 0 {
				t.Errorf("we sent a BYE for a call the far end ended (%d)", n)
			}
			closeBounded(t, u)
		})
	}
}

func TestUnknownByeGets481(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	dialogs := make(chan *siptest.Dialog, 1)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		d, err := tr.Accept(req, tx, answerBody())
		if err == nil {
			dialogs <- d
		}
	})
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := (<-dialogs).ByeUnknownDialog(ctx)
	if err != nil {
		t.Fatalf("trunk BYE: %v", err)
	}
	if res.StatusCode != 481 {
		t.Errorf("BYE for an unknown dialog answered %d, want 481", res.StatusCode)
	}
	select {
	case c := <-r.Call.Ended():
		t.Errorf("a BYE for another dialog ended the call: %s", c)
	default:
	}
}

func TestTransportLostOnClosedConnection(t *testing.T) {
	for _, tp := range []string{"tcp", "tls"} {
		t.Run(tp, func(t *testing.T) {
			old := callKeepaliveEvery
			callKeepaliveEvery = 100 * time.Millisecond
			t.Cleanup(func() { callKeepaliveEvery = old })

			tr := siptest.NewTrunk(t, tp)
			accepted := make(chan *gosip.Request, 1)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				if _, err := tr.Accept(req, tx, answerBody()); err == nil {
					accepted <- req
				}
			})
			u := newTestUA(t, tr, nil)
			r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
			if r.Call == nil {
				t.Fatalf("result = %+v", r)
			}
			// While the connection is healthy the pings keep flowing and the
			// call stays up.
			eventually(t, "keepalive pings", func() bool { return tr.Keepalives() >= 2 })
			select {
			case c := <-r.Call.Ended():
				t.Fatalf("call ended while healthy: %s", c)
			default:
			}
			req := <-accepted
			if !tr.CloseConnection(req) {
				t.Fatal("trunk found no connection to close")
			}
			select {
			case cause := <-r.Call.Ended():
				if cause != TransportLost {
					t.Errorf("Ended = %q", cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no TransportLost after the trunk closed the connection")
			}
			closeBounded(t, u)
		})
	}
}

func TestAbandonReturnsImmediately(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				<-release // never answers
			})
			u := newTestUA(t, tr, nil)
			a := start(t, u, dialRequest(tr, nil))
			if _, ok := tr.WaitRequest("INVITE", 1, 5*time.Second); !ok {
				t.Fatal("no INVITE")
			}
			begin := time.Now()
			a.Abandon()
			r := result(t, a, 50*time.Millisecond)
			if r.Cause != FailAbandoned {
				t.Fatalf("result = %+v", r)
			}
			if d := time.Since(begin); d > 50*time.Millisecond {
				t.Errorf("took %v", d)
			}
			// No 1xx ever came, so no CANCEL is allowed yet.
			time.Sleep(100 * time.Millisecond)
			if n := len(tr.RequestsFor("CANCEL")); n != 0 {
				t.Errorf("%d CANCELs without a provisional response", n)
			}
			noSecondResult(t, a)
		})
	}
}

func TestResultIsSingleUnderConcurrentAbandon(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnBye(byeOK)
	tr.OnInvite(answer200(tr))
	u := newTestUA(t, tr, nil)
	for i := 0; i < 20; i++ {
		a := start(t, u, dialRequest(tr, nil))
		go a.Abandon()
		first := result(t, a, 5*time.Second)
		noSecondResult(t, a)
		if first.Call != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			first.Call.Hangup(ctx)
			cancel()
		}
	}
	closeBounded(t, u)
	// Every dialog the trunk accepted was ended: ACKs and BYEs balance.
	acks, byes := len(tr.RequestsFor("ACK")), len(tr.RequestsFor("BYE"))
	if acks != byes {
		t.Errorf("%d ACKs but %d BYEs: a dialog was left up", acks, byes)
	}
}

func TestDialLateFork(t *testing.T) {
	// The second 2xx arrives after the call was reported: it is cleaned up by
	// the transport-layer tap, not by the attempt goroutine's decision.
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnBye(byeOK)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		tr.RespondRaw(req, 200, "OK", "fork-a", answerBody())
		time.Sleep(300 * time.Millisecond)
		tr.RespondRaw(req, 200, "OK", "fork-b", answerBody())
	})
	u := newTestUA(t, tr, nil)
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("result = %+v", r)
	}
	if tag := toTag(r.Call.(*call).sess.InviteResponse); tag != "fork-a" {
		t.Fatalf("kept dialog %q, want fork-a", tag)
	}
	bye, ok := tr.WaitRequest("BYE", 1, 10*time.Second)
	if !ok || !strings.Contains(bye.Header("To"), "fork-b") {
		t.Fatalf("BYE = %+v, want one for fork-b", bye)
	}
	eventually(t, "ACK for fork-b", func() bool {
		for _, q := range tr.RequestsFor("ACK") {
			if strings.Contains(q.Header("To"), "fork-b") {
				return true
			}
		}
		return false
	})
	closeBounded(t, u)
}
