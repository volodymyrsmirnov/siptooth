package sip

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

func newTestUA(t *testing.T, tr *siptest.Trunk, mod func(*Options)) *UA {
	t.Helper()
	o := Options{
		Transport: tr.Transport(),
		NextHop:   tr.HostPort(),
		UserAgent: "siptooth/test",
	}
	if tr.Transport() == "tls" {
		o.TLSConfig = tr.ClientTLSConfig()
	}
	if mod != nil {
		mod(&o)
	}
	u, err := NewUA(o)
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { u.Close() })
	return u
}

func TestNewUAValidatesOptions(t *testing.T) {
	cases := map[string]Options{
		"bad transport": {Transport: "sctp", NextHop: "127.0.0.1:5060"},
		"no next hop":   {Transport: "udp"},
		"no port":       {Transport: "udp", NextHop: "127.0.0.1"},
		"bad local":     {Transport: "udp", NextHop: "127.0.0.1:5060", LocalPort: 70000},
	}
	for name, o := range cases {
		if u, err := NewUA(o); err == nil {
			u.Close()
			t.Errorf("%s: NewUA succeeded", name)
		}
	}
}

func TestPreferIPv4(t *testing.T) {
	v6 := netip.MustParseAddr("2001:db8::1")
	v4 := netip.MustParseAddr("192.0.2.9")
	if got := preferIPv4([]netip.Addr{v6, v4}); got != v4 {
		t.Errorf("preferIPv4 = %v, want %v", got, v4)
	}
	if got := preferIPv4([]netip.Addr{v6}); got != v6 {
		t.Errorf("preferIPv4 (only v6) = %v, want %v", got, v6)
	}
	mapped := netip.MustParseAddr("::ffff:192.0.2.9")
	if got := preferIPv4([]netip.Addr{mapped}); got != v4 {
		t.Errorf("preferIPv4 (4in6) = %v, want %v", got, v4)
	}
}

func TestResolveNextHop(t *testing.T) {
	res := siptest.NewResolver(t, map[string]netip.Addr{"node1.localhost": netip.MustParseAddr("127.0.0.1")})
	u, err := NewUA(Options{Transport: "tcp", NextHop: "node1.localhost:5099", Resolver: res})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	got, err := u.ResolveNextHop(context.Background())
	if err != nil {
		t.Fatalf("ResolveNextHop: %v", err)
	}
	if want := netip.MustParseAddrPort("127.0.0.1:5099"); got != want {
		t.Errorf("ResolveNextHop = %v, want %v", got, want)
	}

	lit, err := NewUA(Options{Transport: "tcp", NextHop: "[2001:db8::7]:5060"})
	if err != nil {
		t.Fatal(err)
	}
	defer lit.Close()
	got, err = lit.ResolveNextHop(context.Background())
	if err != nil || got != netip.MustParseAddrPort("[2001:db8::7]:5060") {
		t.Errorf("ResolveNextHop literal = %v, %v", got, err)
	}
}

func TestDiscoverContactFromVia(t *testing.T) {
	for _, transport := range []string{"udp", "tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
				res := gosip.NewResponseFromRequest(req, 200, "OK", nil)
				via := res.Via()
				via.Params.Add("received", "198.51.100.7")
				via.Params.Add("rport", "40000")
				tx.Respond(res)
			})
			u := newTestUA(t, tr, nil)

			got, ok := u.DiscoverContact(context.Background(), tr.URI("600"))
			if !ok {
				t.Fatal("DiscoverContact: not ok")
			}
			if want := netip.MustParseAddrPort("198.51.100.7:40000"); got != want {
				t.Errorf("DiscoverContact = %v, want %v", got, want)
			}

			opt, ok := tr.WaitRequest("OPTIONS", 1, time.Second)
			if !ok {
				t.Fatal("trunk saw no OPTIONS")
			}
			if via := opt.Header("Via"); !contains(via, "rport") {
				t.Errorf("OPTIONS Via lacks rport: %q", via)
			}
			if ua := opt.Header("User-Agent"); ua != "siptooth/test" {
				t.Errorf("User-Agent = %q", ua)
			}
			if opt.RequestURI == "" || !contains(opt.RequestURI, "600@") {
				t.Errorf("Request-URI = %q", opt.RequestURI)
			}
		})
	}
}

func TestDiscoverContactErrorResponseStillCounts(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
		res := gosip.NewResponseFromRequest(req, 405, "Method Not Allowed", nil)
		res.Via().Params.Add("received", "203.0.113.5")
		res.Via().Params.Add("rport", "1234")
		tx.Respond(res)
	})
	u := newTestUA(t, tr, nil)
	got, ok := u.DiscoverContact(context.Background(), tr.URI("600"))
	if !ok || got != netip.MustParseAddrPort("203.0.113.5:1234") {
		t.Errorf("DiscoverContact = %v, %v; any status code must be usable", got, ok)
	}
}

func TestDiscoverContactUnusableVia(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
		res := gosip.NewResponseFromRequest(req, 200, "OK", nil)
		res.Via().Params.Remove("received")
		res.Via().Params.Remove("rport")
		tx.Respond(res)
	})
	u := newTestUA(t, tr, nil)
	if got, ok := u.DiscoverContact(context.Background(), tr.URI("600")); ok {
		t.Errorf("DiscoverContact = %v, true; want false without received/rport", got)
	}
}

func TestDiscoverContactTimeoutFallback(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
				<-release // never answer
			})
			u := newTestUA(t, tr, nil)

			start := time.Now()
			_, ok := u.DiscoverContact(context.Background(), tr.URI("600"))
			elapsed := time.Since(start)
			if ok {
				t.Fatal("DiscoverContact ok without a response")
			}
			if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
				t.Errorf("DiscoverContact took %v, want about 1s", elapsed)
			}
		})
	}
}

func TestDiscoverContactHonoursContext(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) { <-release })
	u := newTestUA(t, tr, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := u.DiscoverContact(ctx, tr.URI("600")); ok {
		t.Fatal("DiscoverContact ok")
	}
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Errorf("DiscoverContact ignored the caller's context: %v", d)
	}
}

func TestTLSServerNamePerDestination(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	port := strconv.Itoa(int(tr.Addr().Port()))
	loop := netip.MustParseAddr("127.0.0.1")
	res := siptest.NewResolver(t, map[string]netip.Addr{
		siptest.HostLocalhost: loop,
		siptest.HostNode1:     loop,
	})

	// The UA's TLS config has no ServerName: sipgo must derive it from each
	// destination. The node1 certificate is valid for node1.localhost only
	// and the localhost certificate for localhost (and 127.0.0.1) only, so a
	// handshake verifies only if ServerName followed the destination host.
	u := newTestUA(t, tr, func(o *Options) {
		o.NextHop = siptest.HostNode1 + ":" + port
		o.Resolver = res
	})
	if _, ok := u.DiscoverContact(context.Background(), "sip:600@"+siptest.HostNode1+";transport=tls"); !ok {
		t.Fatal("OPTIONS to node1.localhost failed: certificate not verified for that host")
	}

	// Second destination, second UA. (One UA would reuse the open connection:
	// sipgo keys its connection pool by resolved IP:port, so a second host
	// name resolving to the same address never reaches a new handshake.)
	u2 := newTestUA(t, tr, func(o *Options) {
		o.NextHop = siptest.HostLocalhost + ":" + port
		o.Resolver = res
	})
	if _, ok := u2.DiscoverContact(context.Background(), "sip:600@"+siptest.HostLocalhost+";transport=tls"); !ok {
		t.Fatal("OPTIONS to localhost failed: certificate not verified for that host")
	}

	got := tr.ServerNames()
	if len(got) != 2 || got[0] != siptest.HostNode1 || got[1] != siptest.HostLocalhost {
		t.Errorf("SNI seen by the trunk = %v, want [node1.localhost localhost]", got)
	}
}

func TestTLSRejectsUntrustedServer(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	u := newTestUA(t, tr, func(o *Options) { o.TLSConfig = &tls.Config{} }) // system roots only
	if _, ok := u.DiscoverContact(context.Background(), tr.URI("600")); ok {
		t.Error("handshake with a certificate from an unknown CA must fail")
	}
}

func TestLocalPortIsUsedForSignaling(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			port := freePort(t, transport)
			u := newTestUA(t, tr, func(o *Options) { o.LocalPort = port })

			u.DiscoverContact(context.Background(), tr.URI("600"))
			opt, ok := tr.WaitRequest("OPTIONS", 1, time.Second)
			if !ok {
				t.Fatal("no OPTIONS")
			}
			_, srcPort, _ := net.SplitHostPort(opt.Source)
			if srcPort != strconv.Itoa(port) {
				t.Errorf("source port = %s, want %d", srcPort, port)
			}
			if via := opt.Header("Via"); !contains(via, ":"+strconv.Itoa(port)) {
				t.Errorf("Via sent-by lacks local port %d: %q", port, via)
			}
		})
	}
}

func TestUDPViaCarriesRport(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, nil)
	u.DiscoverContact(context.Background(), tr.URI("600"))
	opt, _ := tr.WaitRequest("OPTIONS", 1, time.Second)
	if !contains(opt.Header("Via"), "rport") {
		t.Errorf("Via = %q, want rport (WithClientNAT)", opt.Header("Via"))
	}
}

func TestWriteTimeoutBoundsStuckConnection(t *testing.T) {
	if tcpWriteTimeout != 5*time.Second {
		t.Fatalf("tcpWriteTimeout = %v, want 5s (spec 5.2)", tcpWriteTimeout)
	}
	old := tcpWriteTimeout
	tcpWriteTimeout = 300 * time.Millisecond
	defer func() { tcpWriteTimeout = old }()

	// A peer that accepts and never reads: socket buffers fill, writes block.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		<-stop
		c.Close()
	}()

	u, err := NewUA(Options{Transport: "tcp", NextHop: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	req := gosip.NewRequest(gosip.MESSAGE, gosip.Uri{Scheme: "sip", User: "x", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port})
	req.SetTransport("TCP")
	req.SetBody(make([]byte, 64<<20))
	req.AppendHeader(gosip.NewHeader("Content-Type", "text/plain"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		_, err := u.Client().Do(ctx, req)
		errc <- err
	}()
	select {
	case err = <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("write to a stuck peer is not bounded by the write timeout")
	}
	if err == nil {
		t.Fatal("write to a stuck peer succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("write took %v to fail, want ~%v", d, tcpWriteTimeout)
	}
	if ctx.Err() != nil {
		t.Errorf("the failure came from the caller's context, not the write deadline: %v", err)
	}
}

func TestUAExposesComponents(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	u := newTestUA(t, tr, nil)
	if u.Dialogs() == nil || u.Client() == nil || u.Server() == nil {
		t.Fatal("UA must expose client, server and dialog cache")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

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

func TestUDPViaNeverCarriesWildcardAddress(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	u := newTestUA(t, tr, nil) // LocalPort 0: ephemeral port, but a concrete address
	u.DiscoverContact(context.Background(), tr.URI("600"))
	opt, ok := tr.WaitRequest("OPTIONS", 1, time.Second)
	if !ok {
		t.Fatal("no OPTIONS")
	}
	via := opt.Header("Via")
	if strings.Contains(via, "0.0.0.0") || !strings.Contains(via, "127.0.0.1:") {
		t.Errorf("Via = %q, want the local address toward the next hop", via)
	}
	_, srcPort, _ := net.SplitHostPort(opt.Source)
	if !strings.Contains(via, ":"+srcPort) || srcPort == "0" {
		t.Errorf("Via = %q does not carry the real source port %s", via, srcPort)
	}
}

func TestServerNameInTLSConfigIsIgnored(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	res := siptest.NewResolver(t, map[string]netip.Addr{siptest.HostNode1: netip.MustParseAddr("127.0.0.1")})
	u := newTestUA(t, tr, func(o *Options) {
		o.TLSConfig = tr.ClientTLSConfig()
		o.TLSConfig.ServerName = "wrong.example" // would break every verification
		o.NextHop = siptest.HostNode1 + ":" + strconv.Itoa(int(tr.Addr().Port()))
		o.Resolver = res
	})
	if _, ok := u.DiscoverContact(context.Background(), "sip:600@"+siptest.HostNode1+";transport=tls"); !ok {
		t.Fatal("handshake failed: a caller-supplied ServerName leaked through")
	}
	if got := tr.ServerNames(); len(got) != 1 || got[0] != siptest.HostNode1 {
		t.Errorf("SNI = %v, want the destination host", got)
	}
}
