package bridge

import (
	"context"
	"encoding/binary"
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

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/nat"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

type errorSink struct {
	mu    sync.Mutex
	codes []string
}

func (e *errorSink) EmitError(code, detail string) {
	e.mu.Lock()
	e.codes = append(e.codes, code)
	e.mu.Unlock()
}

func (e *errorSink) count(code string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.codes {
		if c == code {
			n++
		}
	}
	return n
}

func adapterConfig(tr *siptest.Trunk) config.Config {
	cfg := config.Default()
	cfg.SIP.Transport = tr.Transport()
	cfg.SIP.Target = tr.URI("200")
	cfg.NAT.RTPPortMin, cfg.NAT.RTPPortMax = 41000, 42000
	return cfg
}

func adapterUA(t *testing.T, tr *siptest.Trunk) *sip.UA {
	t.Helper()
	o := sip.Options{Transport: tr.Transport(), NextHop: tr.HostPort(), UserAgent: "siptooth/test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func newAdapter(t *testing.T, tr *siptest.Trunk, mod func(*AdapterDeps)) (*SIPAdapter, *errorSink) {
	t.Helper()
	sink := &errorSink{}
	d := AdapterDeps{UA: adapterUA(t, tr), Config: adapterConfig(tr), Hooks: sink,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&d)
	}
	return NewSIPAdapter(d), sink
}

func TestPrepareLocalModeForLoopbackTarget(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	a, sink := newAdapter(t, tr, nil)

	p, err := a.Prepare(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Media.Close()

	if p.NAT.Mode != nat.ModeLocal || p.NAT.STUNServer != "" || p.NAT.STUNFailed {
		t.Fatalf("decision = %+v, want local without STUN", p.NAT)
	}
	rs := p.Media.(rtpMedia).LocalAddr()
	if p.Offer.Addr != rs || !rs.Addr().IsLoopback() || rs.Port()%2 != 0 || rs.Port() < 41000 || rs.Port() > 42000 {
		t.Fatalf("offer addr %v, socket %v", p.Offer.Addr, rs)
	}
	if len(p.Offer.Codecs) != 2 || p.Offer.LocalKey != nil || p.LocalKey != nil {
		t.Fatalf("offer %+v key %v", p.Offer, p.LocalKey)
	}
	if want := "sip:siptooth@127.0.0.1:5060;transport=udp"; p.Contact != want {
		t.Fatalf("contact = %q, want %q", p.Contact, want)
	}
	if n := len(tr.RequestsFor("OPTIONS")); n != 0 {
		t.Fatalf("local mode sent %d OPTIONS", n)
	}
	if sink.count("stun_failed") != 0 {
		t.Fatal("stun_failed emitted in local mode")
	}
}

func TestPrepareSRTPGeneratesKey(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	a, _ := newAdapter(t, tr, func(d *AdapterDeps) { d.Config.SIP.SRTP = true })
	p, err := a.Prepare(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Media.Close()
	if len(p.LocalKey) != 30 || string(p.Offer.LocalKey) != string(p.LocalKey) {
		t.Fatalf("key %d bytes, offer key equal %v", len(p.LocalKey), string(p.Offer.LocalKey) == string(p.LocalKey))
	}
	if !strings.Contains(p.Contact, "transport=tls") {
		t.Fatalf("contact %q", p.Contact)
	}
}

func TestPrepareHonorsCancelledContext(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	a, _ := newAdapter(t, tr, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := a.Prepare(ctx, "127.0.0.1")
	if err == nil {
		p.Media.Close()
		t.Fatal("cancelled context: want an error")
	}
}

func TestPrepareRegisteredContactWins(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	flow := netip.MustParseAddrPort("203.0.113.5:5070")
	a, _ := newAdapter(t, tr, func(d *AdapterDeps) {
		d.Config.SIP.Register, d.Config.SIP.Username = true, "1234-100"
		d.Registration = func() (sip.RegState, bool) { return sip.RegState{Registered: true, Contact: flow}, true }
		d.Config.NAT.Mode = "public_ip" // not local: would otherwise use OPTIONS
		d.Config.NAT.PublicIP = "198.51.100.7"
	})
	p, err := a.Prepare(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Media.Close()
	if want := "sip:1234-100@203.0.113.5:5070;transport=udp"; p.Contact != want {
		t.Fatalf("contact = %q, want %q", p.Contact, want)
	}
	if p.Offer.Addr.Addr() != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("public_ip not advertised: %v", p.Offer.Addr)
	}
	if n := len(tr.RequestsFor("OPTIONS")); n != 0 {
		t.Fatalf("registered contact still sent %d OPTIONS", n)
	}
}

func TestPrepareDiscoversContactWhenNotLocal(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	a, _ := newAdapter(t, tr, func(d *AdapterDeps) {
		d.Config.NAT.Mode = "public_ip"
		d.Config.NAT.PublicIP = "198.51.100.7"
	})
	p, err := a.Prepare(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Media.Close()
	if n := len(tr.RequestsFor("OPTIONS")); n != 1 {
		t.Fatalf("OPTIONS sent = %d, want 1", n)
	}
	// Loopback flow: the trunk saw us at 127.0.0.1 with our UDP source port.
	if !strings.HasPrefix(p.Contact, "sip:siptooth@127.0.0.1:") || strings.HasSuffix(p.Contact, ":5060;transport=udp") {
		t.Fatalf("contact = %q, want the discovered flow", p.Contact)
	}
}

// stunMock answers Binding Requests with mapped (silent when mapped is
// invalid) and counts them.
func stunMock(t *testing.T, mapped netip.AddrPort) (string, *atomic.Int32) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var n atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			k, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			n.Add(1)
			if !mapped.IsValid() {
				continue
			}
			r := make([]byte, 32)
			binary.BigEndian.PutUint16(r[0:], 0x0101)
			binary.BigEndian.PutUint16(r[2:], 12)
			copy(r[4:], buf[4:k][:16])
			binary.BigEndian.PutUint16(r[20:], 0x0020)
			binary.BigEndian.PutUint16(r[22:], 8)
			r[25] = 1
			binary.BigEndian.PutUint16(r[26:], mapped.Port()^0x2112)
			ip := mapped.Addr().As4()
			for i, cb := range []byte{0x21, 0x12, 0xa4, 0x42} {
				r[28+i] = ip[i] ^ cb
			}
			c.WriteToUDPAddrPort(r, from)
		}
	}()
	return c.LocalAddr().String(), &n
}

func TestPrepareSTUNModeAdvertisesMappingAndKeepsItAlive(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	mapped := netip.MustParseAddrPort("198.51.100.9:40002")
	server, requests := stunMock(t, mapped)
	a, sink := newAdapter(t, tr, func(d *AdapterDeps) {
		d.Config.NAT.Mode = "stun"
		d.Config.NAT.StunServers = []string{server}
		d.STUNKeepalive = 40 * time.Millisecond
	})
	p, err := a.Prepare(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Media.Close()
	if p.NAT.Mode != nat.ModeSTUN || p.NAT.STUNServer != server || p.Offer.Addr != mapped {
		t.Fatalf("decision %+v offer %v", p.NAT, p.Offer.Addr)
	}
	waitUntil(t, "keepalive Binding Requests", func() bool { return requests.Load() >= 3 })
	if sink.count("stun_failed") != 0 {
		t.Fatal("stun_failed on success")
	}
	// Closing the media stops the keepalive.
	p.Media.Close()
	time.Sleep(60 * time.Millisecond)
	n := requests.Load()
	time.Sleep(150 * time.Millisecond)
	if requests.Load() != n {
		t.Fatal("keepalive kept running after Media.Close")
	}
}

func TestSTUNFailedRateLimited(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	server, _ := stunMock(t, netip.AddrPort{}) // never answers
	clock := newFakeClock()
	a, sink := newAdapter(t, tr, func(d *AdapterDeps) {
		d.Config.NAT.Mode = "stun"
		d.Config.NAT.StunServers = []string{server}
		d.STUNTimeout = 20 * time.Millisecond
		d.Clock = clock
	})
	prepare := func() {
		t.Helper()
		p, err := a.Prepare(context.Background(), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if !p.NAT.STUNFailed || p.Offer.Addr != p.Media.(rtpMedia).LocalAddr() {
			t.Fatalf("decision %+v: want STUN failure falling back to the local address", p.NAT)
		}
		p.Media.Close()
	}
	prepare()
	prepare()
	if n := sink.count("stun_failed"); n != 1 {
		t.Fatalf("after two failures in a row: %d system.error, want 1", n)
	}
	clock.Advance(59 * time.Minute)
	prepare()
	if n := sink.count("stun_failed"); n != 1 {
		t.Fatalf("59 minutes later: %d, want still 1", n)
	}
	clock.Advance(2 * time.Minute)
	prepare()
	if n := sink.count("stun_failed"); n != 2 {
		t.Fatalf("an hour later: %d, want 2", n)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAdapterDialAndMedia(t *testing.T) {
	for _, tp := range []string{"udp", "tls"} {
		t.Run(tp, func(t *testing.T) {
			tr := siptest.NewTrunk(t, tp)
			echo := siptest.RTPEcho(t)
			tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
				body := strings.Join([]string{
					"v=0", "o=pbx 1 1 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
					fmt.Sprintf("m=audio %d RTP/AVP 0", echo.Port()), "a=rtpmap:0 PCMU/8000", "",
				}, "\r\n")
				tr.Accept(req, tx, []byte(body))
			})
			a, _ := newAdapter(t, tr, nil)

			p, err := a.Prepare(context.Background(), "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Media.Close()
			att, err := a.Start(sip.DialRequest{
				Target: tr.URI("200"), From: "sip:siptooth@example.test", Contact: p.Contact,
				Offer: p.Offer, NextHop: tr.HostPort(), Transport: tp,
			})
			if err != nil {
				t.Fatal(err)
			}
			var res sip.DialResult
			select {
			case res = <-att.Result():
			case <-time.After(5 * time.Second):
				t.Fatal("no dial result")
			}
			if res.Call == nil {
				t.Fatalf("dial failed: %+v", res)
			}
			defer res.Call.Hangup(context.Background())

			if err := p.Media.Start(res.Call.Remote(), p.LocalKey); err != nil {
				t.Fatal(err)
			}
			frame := make([]int16, 160)
			for i := range frame {
				frame[i] = int16(2000 * (i%16 - 8))
			}
			waitUntil(t, "echoed audio in the playout", func() bool {
				if err := p.Media.WriteFrame(frame); err != nil {
					t.Fatal(err)
				}
				return p.Media.Stats().Received > 0 && p.Media.Playout() != nil && p.Media.Playout().Depth() > 0
			})
			out := make([]int16, 160)
			var nonzero bool
			for i := 0; i < 20 && !nonzero; i++ {
				p.Media.Playout().Read(out)
				for _, s := range out {
					if s != 0 {
						nonzero = true
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !nonzero {
				t.Fatal("echoed frames never came back as audio")
			}
			if got := p.Media.Stats().Sent; got == 0 {
				t.Fatal("no RTP sent")
			}
		})
	}
}
