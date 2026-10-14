package dnsv4_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/dnsv4"
)

func TestResolverReturnsAOnly(t *testing.T) {
	f := startFakeDNS(t, aaaaNXDomain, map[string]net.IP{"sip.test.": net.ParseIP("136.112.48.140")}, nil)
	r := dnsv4.Resolver(dialTo(f.addr))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	addrs, err := r.LookupIPAddr(ctx, "sip.test")
	if err != nil || len(addrs) != 1 || addrs[0].IP.String() != "136.112.48.140" {
		t.Fatalf("LookupIPAddr = %v, %v", addrs, err)
	}
	nips, err := r.LookupNetIP(ctx, "ip", "sip.test")
	if err != nil || len(nips) != 1 || nips[0].String() != "136.112.48.140" {
		t.Fatalf("LookupNetIP = %v, %v", nips, err)
	}
	if f.sawType(28) {
		t.Fatalf("AAAA query reached upstream: %v", f.seen())
	}
}

func TestResolverNeverSendsAAAAEvenIfUpstreamHangs(t *testing.T) {
	f := startFakeDNS(t, aaaaDrop, map[string]net.IP{"sip.test.": net.ParseIP("10.1.2.3")}, nil)
	r := dnsv4.Resolver(dialTo(f.addr))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.LookupNetIP(ctx, "ip", "sip.test"); err != nil {
		t.Fatal(err)
	}
}

func TestDialerByHostname(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	f := startFakeDNS(t, aaaaNXDomain, map[string]net.IP{"sip.test.": net.ParseIP("127.0.0.1")}, nil)
	d := net.Dialer{Resolver: dnsv4.Resolver(dialTo(f.addr)), Timeout: 3 * time.Second}
	c, err := d.Dial("tcp", net.JoinHostPort("sip.test", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAAAAOnlyNameIsCleanNoSuchHost(t *testing.T) {
	f := startFakeDNS(t, aaaaNXDomain, nil, map[string]net.IP{"v6.test.": net.ParseIP("2001:db8::1")})
	r := dnsv4.Resolver(dialTo(f.addr))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.LookupNetIP(ctx, "ip", "v6.test")
	var de *net.DNSError
	if !errors.As(err, &de) || !de.IsNotFound {
		t.Fatalf("err = %v, want no such host", err)
	}
}

// pipeConn is a net.Conn stub recording writes; used for the wire-level tests.
type recConn struct {
	net.Conn
	written [][]byte
	read    []byte
}

func (c *recConn) Write(b []byte) (int, error) {
	c.written = append(c.written, append([]byte(nil), b...))
	return len(b), nil
}
func (c *recConn) Read(b []byte) (int, error) { return copy(b, c.read), nil }
func (c *recConn) Close() error               { return nil }

// recPacket additionally satisfies net.PacketConn, so it is treated as UDP.
type recPacket struct{ recConn }

func (c *recPacket) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, nil }
func (c *recPacket) WriteTo([]byte, net.Addr) (int, error)  { return 0, nil }
func (c *recPacket) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (c *recPacket) SetDeadline(time.Time) error            { return nil }
func (c *recPacket) SetReadDeadline(time.Time) error        { return nil }
func (c *recPacket) SetWriteDeadline(time.Time) error       { return nil }

func query(id uint16, qtype uint16) []byte {
	q := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0,
		3, 'a', 'b', 'c', 0}
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(q, qtype), 1)
}

func TestWrapAAAASynthesizesNoData(t *testing.T) {
	under := &recPacket{}
	c := dnsv4.WrapConn(under)
	q := query(0xBEEF, 28)
	if n, err := c.Write(q); err != nil || n != len(q) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if len(under.recConn.written) != 0 {
		t.Fatal("AAAA was forwarded")
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	r := buf[:n]
	if binary.BigEndian.Uint16(r) != 0xBEEF {
		t.Fatal("id not copied")
	}
	flags := binary.BigEndian.Uint16(r[2:])
	if flags&0x8000 == 0 || flags&0x0100 == 0 || flags&0x0080 == 0 || flags&0x000F != 0 {
		t.Fatalf("flags = %#x", flags)
	}
	if binary.BigEndian.Uint16(r[4:]) != 1 || binary.BigEndian.Uint16(r[6:]) != 0 {
		t.Fatal("counts wrong")
	}
	if string(r[12:]) != string(q[12:]) {
		t.Fatal("question not echoed")
	}
}

func TestWrapTCPAAAAUsesLengthPrefix(t *testing.T) {
	under := &recConn{}
	c := dnsv4.WrapConn(under)
	q := query(7, 28)
	framed := append([]byte{byte(len(q) >> 8), byte(len(q))}, q...)
	if _, err := c.Write(framed); err != nil {
		t.Fatal(err)
	}
	if len(under.written) != 0 {
		t.Fatal("AAAA was forwarded")
	}
	hdr := make([]byte, 2)
	if _, err := c.Read(hdr); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(hdr[0])<<8|int(hdr[1]))
	if n, err := c.Read(body); err != nil || n != len(body) {
		t.Fatalf("body read %d %v", n, err)
	}
	if binary.BigEndian.Uint16(body) != 7 {
		t.Fatal("id")
	}
}

func TestWrapPassesThroughOtherAndMalformed(t *testing.T) {
	under := &recPacket{}
	c := dnsv4.WrapConn(under)
	cases := [][]byte{
		query(1, 1),                        // A
		nil,                                // empty
		{1, 2, 3},                          // short
		query(2, 28)[:14],                  // truncated question
		append(query(3, 28)[:12:12], 0xFF), // label overruns
	}
	for _, p := range cases {
		if _, err := c.Write(p); err != nil {
			t.Fatalf("Write(%x): %v", p, err)
		}
	}
	if len(under.recConn.written) != len(cases) {
		t.Fatalf("forwarded %d of %d", len(under.recConn.written), len(cases))
	}
	for i, p := range cases {
		if string(under.recConn.written[i]) != string(p) {
			t.Fatalf("case %d altered", i)
		}
	}
}
