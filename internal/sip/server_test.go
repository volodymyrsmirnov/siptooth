package sip

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

// reofferBody is a plain RTP PCMU re-offer from the far end.
func reofferBody(port int, direction string) []byte {
	lines := []string{
		"v=0", "o=pbx 1 2 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP 0", port), "a=rtpmap:0 PCMU/8000",
	}
	if direction != "" {
		lines = append(lines, "a="+direction)
	}
	return []byte(strings.Join(append(lines, ""), "\r\n"))
}

// establish dials the trunk and returns the answered call with the trunk's
// view of the dialog.
func establish(t *testing.T, tp string, mod func(*UA)) (*siptest.Trunk, *UA, Call, *siptest.Dialog) {
	t.Helper()
	tr := siptest.NewTrunk(t, tp)
	dialogs := make(chan *siptest.Dialog, 1)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if d, err := tr.Accept(req, tx, answerBody()); err == nil {
			dialogs <- d
		}
	})
	tr.OnBye(byeOK)
	u := newTestUA(t, tr, nil)
	if mod != nil {
		mod(u)
	}
	r := result(t, start(t, u, dialRequest(tr, nil)), 5*time.Second)
	if r.Call == nil {
		t.Fatalf("dial result = %+v", r)
	}
	select {
	case d := <-dialogs:
		return tr, u, r.Call, d
	case <-time.After(5 * time.Second):
		t.Fatal("trunk dialog never created")
		return nil, nil, nil, nil
	}
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// oVersion returns the o= version of an SDP body.
func oVersion(t *testing.T, body []byte) string {
	t.Helper()
	for _, l := range strings.Split(string(body), "\r\n") {
		if strings.HasPrefix(l, "o=") {
			if f := strings.Fields(l); len(f) >= 3 {
				return f[2]
			}
		}
	}
	t.Fatalf("no o= line in %q", body)
	return ""
}

func noMediaUpdate(t *testing.T, c Call) {
	t.Helper()
	select {
	case m := <-c.MediaUpdates():
		t.Errorf("unexpected media update %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestReinviteRefresh200(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			_, u, c, d := establish(t, tp, nil)
			res, err := d.ReInvite(ctx5(t), reofferBody(30000, "sendrecv"))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("re-INVITE = %v, %v", res, err)
			}
			body := res.Body()
			if !strings.Contains(string(body), "m=audio 20000 RTP/AVP 0\r\n") || strings.Contains(string(body), " 8") {
				t.Errorf("answer = %q, want our address and only PCMU", body)
			}
			if v := oVersion(t, body); v != "2" {
				t.Errorf("o= version = %s, want 2", v)
			}
			if ct := res.GetHeader("Content-Type"); ct == nil || ct.Value() != "application/sdp" {
				t.Errorf("Content-Type = %v", ct)
			}
			if res.Contact() == nil {
				t.Error("no Contact in the 200")
			}
			// A second refresh increments again.
			res, err = d.ReInvite(ctx5(t), reofferBody(30000, ""))
			if err != nil || res.StatusCode != 200 || oVersion(t, res.Body()) != "3" {
				t.Fatalf("second re-INVITE = %v, %v", res, err)
			}
			noMediaUpdate(t, c)
			if got := c.Remote().Addr; got != netip.MustParseAddrPort("127.0.0.1:30000") {
				t.Errorf("Remote changed to %v", got)
			}
			closeBounded(t, u)
		})
	}
}

func TestReinviteAddressChange(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			_, u, c, d := establish(t, tp, nil)
			res, err := d.ReInviteNoAck(ctx5(t), reofferBody(30002, "sendrecv"))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("re-INVITE = %v, %v", res, err)
			}
			// Not applied before the ACK.
			noMediaUpdate(t, c)
			if got := c.Remote().Addr.Port(); got != 30000 {
				t.Fatalf("address applied before the ACK: %d", got)
			}
			// Now ACK the same CSeq.
			if err := d.Ack(nil); err != nil {
				t.Fatal(err)
			}
			select {
			case m := <-c.MediaUpdates():
				if m.Addr != netip.MustParseAddrPort("127.0.0.1:30002") {
					t.Errorf("update = %+v", m)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no media update after the ACK")
			}
			if got := c.Remote().Addr.Port(); got != 30002 {
				t.Errorf("Remote port = %d", got)
			}
			closeBounded(t, u)
		})
	}
}

func TestReinviteHold488(t *testing.T) {
	for _, dir := range []string{"sendonly", "inactive", "recvonly"} {
		t.Run(dir, func(t *testing.T) {
			_, u, c, d := establish(t, "udp", nil)
			res, err := d.ReInvite(ctx5(t), reofferBody(30009, dir))
			if err != nil || res.StatusCode != 488 {
				t.Fatalf("re-INVITE = %v, %v", res, err)
			}
			noMediaUpdate(t, c)
			if got := c.Remote().Addr.Port(); got != 30000 {
				t.Errorf("Remote port = %d, want 30000", got)
			}
			// The 488 consumed no version: the next accepted answer is 2.
			res, err = d.ReInvite(ctx5(t), reofferBody(30000, ""))
			if err != nil || res.StatusCode != 200 || oVersion(t, res.Body()) != "2" {
				t.Fatalf("follow-up = %v, %v", res, err)
			}
			closeBounded(t, u)
		})
	}
}

func TestReinviteWrongCodec488(t *testing.T) {
	_, u, _, d := establish(t, "udp", nil)
	body := strings.Replace(string(reofferBody(30000, "")), "RTP/AVP 0", "RTP/AVP 18", 1)
	res, err := d.ReInvite(ctx5(t), []byte(body))
	if err != nil || res.StatusCode != 488 {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	closeBounded(t, u)
}

func TestReinviteOfferless(t *testing.T) {
	for _, tp := range allTransports {
		t.Run(tp, func(t *testing.T) {
			_, u, c, d := establish(t, tp, nil)
			res, err := d.ReInviteOfferless(ctx5(t), reofferBody(30004, "sendrecv"))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("re-INVITE = %v, %v", res, err)
			}
			if !strings.Contains(string(res.Body()), "m=audio 20000 RTP/AVP 0") || oVersion(t, res.Body()) != "2" {
				t.Errorf("offer = %q", res.Body())
			}
			select {
			case m := <-c.MediaUpdates():
				if m.Addr.Port() != 30004 {
					t.Errorf("update = %+v", m)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no media update from the SDP in the ACK")
			}
			closeBounded(t, u)
		})
	}
}

func TestReinviteOfferlessBadAckSDP(t *testing.T) {
	_, u, c, d := establish(t, "udp", nil)
	res, err := d.ReInviteOfferless(ctx5(t), reofferBody(30004, "sendonly"))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	noMediaUpdate(t, c)
	if got := c.Remote().Addr.Port(); got != 30000 {
		t.Errorf("Remote port = %d, want 30000", got)
	}
	closeBounded(t, u)
}

func TestReinviteLowCSeq500(t *testing.T) {
	_, u, _, d := establish(t, "udp", nil)
	if res, err := d.ReInviteCSeq(ctx5(t), 50, reofferBody(30000, "")); err != nil || res.StatusCode != 200 {
		t.Fatalf("first = %v, %v", res, err)
	}
	for _, cseq := range []uint32{50, 49} {
		res, err := d.ReInviteCSeq(ctx5(t), cseq, reofferBody(30000, ""))
		if err != nil || res.StatusCode != 500 {
			t.Fatalf("CSeq %d = %v, %v, want 500", cseq, res, err)
		}
	}
	if res, err := d.ReInviteCSeq(ctx5(t), 51, reofferBody(30000, "")); err != nil || res.StatusCode != 200 {
		t.Fatalf("increasing CSeq = %v, %v", res, err)
	}
	closeBounded(t, u)
}

func TestReinviteUDPRetransmitUntilAck(t *testing.T) {
	_, u, _, d := establish(t, "udp", func(u *UA) {
		u.reinviteT1, u.reinviteT2 = 50*time.Millisecond, 200*time.Millisecond
	})
	res, err := d.ReInviteNoAck(ctx5(t), reofferBody(30000, ""))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	cseq := res.CSeq().SeqNo
	count := func() int { return countInviteOKs(d.Trunk(), cseq) }
	eventually(t, "three copies of the 2xx", func() bool { return count() >= 4 })
	if err := d.Ack(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let an in-flight copy land
	n := count()
	time.Sleep(600 * time.Millisecond)
	if after := count(); after != n {
		t.Errorf("2xx still retransmitted after the ACK: %d -> %d", n, after)
	}
	closeBounded(t, u)
}

func countInviteOKs(tr *siptest.Trunk, cseq uint32) int {
	n := 0
	want := fmt.Sprintf("CSeq: %d INVITE", cseq)
	for _, m := range tr.RawResponses() {
		if strings.HasPrefix(m, "SIP/2.0 200") && strings.Contains(m, want) {
			n++
		}
	}
	return n
}

func TestReinviteNoAckByesAndEnds(t *testing.T) {
	tr, u, c, d := establish(t, "udp", func(u *UA) {
		u.reinviteT1, u.reinviteT2 = 20*time.Millisecond, 40*time.Millisecond
		u.reinviteAckTimeout = 400 * time.Millisecond
	})
	res, err := d.ReInviteNoAck(ctx5(t), reofferBody(30006, ""))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("re-INVITE = %v, %v", res, err)
	}
	select {
	case cause := <-c.Ended():
		if cause != TransportLost {
			t.Errorf("Ended = %q, want transport_lost", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call never ended")
	}
	if _, ok := tr.WaitRequest("BYE", 1, 5*time.Second); !ok {
		t.Error("no BYE after the ACK timeout")
	}
	noMediaUpdate(t, c)
	closeBounded(t, u)
}

func TestOptions200(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) { byeOK(req, tx) })
	u := newTestUA(t, tr, nil)
	src := learnFlow(t, tr, u)
	res, err := tr.SendRequest(ctx5(t), "OPTIONS", "sip:siptooth@"+src.String(), nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("OPTIONS = %v, %v", res, err)
	}
	if h := res.GetHeader("Allow"); h == nil || !strings.Contains(h.Value(), "OPTIONS") || strings.Contains(h.Value(), "UPDATE") {
		t.Errorf("Allow = %v", h)
	}
}

// learnFlow makes the UA send one request so the trunk learns where the UA's
// UDP socket is, and returns that address.
func learnFlow(t *testing.T, tr *siptest.Trunk, u *UA) netip.AddrPort {
	t.Helper()
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) { byeOK(req, tx) })
	if _, ok := u.DiscoverContact(ctx5(t), "sip:probe@127.0.0.1"); !ok {
		t.Fatal("probe OPTIONS failed")
	}
	r, ok := tr.WaitRequest("OPTIONS", 1, 5*time.Second)
	if !ok {
		t.Fatal("trunk saw no OPTIONS")
	}
	ap, err := netip.ParseAddrPort(r.Source)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

func TestUnsolicitedInvite403(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, nil)
	src := learnFlow(t, tr, u)
	res, err := tr.SendRequest(ctx5(t), "INVITE", "sip:siptooth@"+src.String(), []byte(string(reofferBody(30000, ""))))
	if err != nil || res.StatusCode != 403 {
		t.Fatalf("INVITE = %v, %v, want 403", res, err)
	}
}

func TestUnknownDialogInvite481(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, nil)
	src := learnFlow(t, tr, u)
	to := gosip.NewHeader("To", "<sip:siptooth@127.0.0.1>;tag=nosuch")
	res, err := tr.SendRequest(ctx5(t), "INVITE", "sip:siptooth@"+src.String(), nil, to)
	if err != nil || res.StatusCode != 481 {
		t.Fatalf("INVITE = %v, %v, want 481", res, err)
	}
}

func TestUpdate501(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, nil)
	src := learnFlow(t, tr, u)
	for _, m := range []string{"UPDATE", "INFO", "NOTIFY", "REFER", "MESSAGE", "PRACK"} {
		res, err := tr.SendRequest(ctx5(t), m, "sip:siptooth@"+src.String(), nil)
		if err != nil || res.StatusCode != 501 {
			t.Errorf("%s = %v, %v, want 501", m, res, err)
		}
	}
}
