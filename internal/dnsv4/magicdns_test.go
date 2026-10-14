package dnsv4_test

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/dnsv4"
)

// Reply captured on the Pi from Tailscale MagicDNS (100.100.100.100) for an A
// query carrying an EDNS0 OPT record: the OPT pseudo-record precedes the
// answer although ANCOUNT=1, ARCOUNT=1, so Go reads the OPT as the answer.
const capturedReply = "bfcc8000000100010000000103736970037274630a656c6576656e6c61627302696f000001000100002904d0000000000000c00c000100010000000b00048870308c"

const magicName = "sip.rtc.elevenlabs.io"

// questionEnd returns the offset just past the first question of q.
func questionEnd(q []byte) int {
	i := 12
	for q[i] != 0 {
		i += 1 + int(q[i])
	}
	return i + 5
}

// magicReply mimics MagicDNS: with an OPT in the query it answers with the
// captured (broken) layout, without one it answers correctly. It records the
// query's ARCOUNT and whether an OPT was present.
func magicReply(q []byte, rec func(arcount uint16)) []byte {
	if len(q) < 17 {
		return nil
	}
	qend := questionEnd(q)
	qtype := binary.BigEndian.Uint16(q[qend-4:])
	arcount := binary.BigEndian.Uint16(q[10:])
	rec(arcount)
	resp := append([]byte(nil), q[:qend]...)
	binary.BigEndian.PutUint16(resp[2:], 0x8000)
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	binary.BigEndian.PutUint16(resp[10:], 0)
	if qtype != 1 {
		return resp // NODATA
	}
	ans := []byte{0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 0x0b, 0, 4, 0x88, 0x70, 0x30, 0x8c}
	binary.BigEndian.PutUint16(resp[6:], 1)
	if arcount == 0 {
		return append(resp, ans...)
	}
	binary.BigEndian.PutUint16(resp[10:], 1)
	opt := []byte{0, 0, 0x29, 0x04, 0xd0, 0, 0, 0, 0, 0, 0}
	resp = append(resp, opt...)
	return append(resp, ans...)
}

type magicDNS struct {
	addr string
	mu   sync.Mutex
	ar   []uint16
}

func (m *magicDNS) record(a uint16) { m.mu.Lock(); m.ar = append(m.ar, a); m.mu.Unlock() }

func (m *magicDNS) arcounts() []uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint16(nil), m.ar...)
}

func startMagicUDP(t *testing.T) *magicDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &magicDNS{addr: pc.LocalAddr().String()}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := magicReply(append([]byte(nil), buf[:n]...), m.record); r != nil {
				pc.WriteTo(r, from)
			}
		}
	}()
	return m
}

func startMagicTCP(t *testing.T) *magicDNS {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &magicDNS{addr: l.Addr().String()}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					var h [2]byte
					if _, err := io.ReadFull(c, h[:]); err != nil {
						return
					}
					q := make([]byte, binary.BigEndian.Uint16(h[:]))
					if _, err := io.ReadFull(c, q); err != nil {
						return
					}
					if r := magicReply(q, m.record); r != nil {
						c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(r))), r...))
					}
				}
			}()
		}
	}()
	return m
}

func TestCapturedReplyLayout(t *testing.T) {
	b, _ := hex.DecodeString(capturedReply)
	if len(b) != 66 {
		t.Fatalf("len = %d", len(b))
	}
	// Our fake must reproduce the capture byte for byte (ID aside).
	q := []byte{0xbf, 0xcc, 0x01, 0x20, 0, 1, 0, 0, 0, 0, 0, 1}
	q = append(q, b[12:12+questionEnd(b)-12]...)
	q = append(q, 0, 0, 0x29, 0x04, 0xd0, 0, 0, 0, 0, 0, 0)
	got := magicReply(q, func(uint16) {})
	if hex.EncodeToString(got) != capturedReply {
		t.Fatalf("fake differs from capture:\n%x\n%s", got, capturedReply)
	}
}

func TestStockGoResolverFailsAgainstMagicDNS(t *testing.T) {
	m := startMagicUDP(t)
	r := &net.Resolver{PreferGo: true, Dial: dialTo(m.addr)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.LookupNetIP(ctx, "ip", magicName)
	if err == nil {
		t.Fatal("stock resolver unexpectedly succeeded; fake does not reproduce the bug")
	}
	t.Logf("stock resolver: %v", err)
}

func checkResolves(t *testing.T, m *magicDNS, network string) {
	t.Helper()
	dial := dialTo(m.addr)
	r := dnsv4.Resolver(func(ctx context.Context, _, address string) (net.Conn, error) {
		return dial(ctx, network, address)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nips, err := r.LookupNetIP(ctx, "ip", magicName)
	if err != nil || len(nips) != 1 || nips[0].String() != "136.112.48.140" {
		t.Fatalf("LookupNetIP = %v, %v", nips, err)
	}
	addrs, err := r.LookupIPAddr(ctx, magicName)
	if err != nil || len(addrs) != 1 || addrs[0].IP.String() != "136.112.48.140" {
		t.Fatalf("LookupIPAddr = %v, %v", addrs, err)
	}
	ar := m.arcounts()
	if len(ar) == 0 {
		t.Fatal("no query reached the server")
	}
	for _, a := range ar {
		if a != 0 {
			t.Fatalf("query reached server with ARCOUNT=%d (OPT not removed): %v", a, ar)
		}
	}
}

func TestResolverStripsEDNS0UDP(t *testing.T) { checkResolves(t, startMagicUDP(t), "udp") }
func TestResolverStripsEDNS0TCP(t *testing.T) { checkResolves(t, startMagicTCP(t), "tcp") }

func TestWrapStripsOnlyLoneOPT(t *testing.T) {
	opt := []byte{0, 0, 0x29, 0x04, 0xd0, 0, 0, 0, 0, 0, 0}
	withOPT := append(query(5, 1), opt...)
	binary.BigEndian.PutUint16(withOPT[10:], 1)

	two := append(append([]byte(nil), withOPT...), opt...) // two additional RRs
	binary.BigEndian.PutUint16(two[10:], 2)
	trailing := append(append([]byte(nil), withOPT...), 0xFF) // junk after the OPT
	notOPT := append([]byte(nil), withOPT...)
	notOPT[len(query(5, 1))+2] = 16 // TXT, not OPT

	under := &recPacket{}
	c := dnsv4.WrapConn(under)
	for _, p := range [][]byte{withOPT, two, trailing, notOPT} {
		if _, err := c.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	w := under.recConn.written
	if string(w[0]) != string(query(5, 1)) {
		t.Fatalf("OPT not stripped: %x", w[0])
	}
	for i, p := range [][]byte{two, trailing, notOPT} {
		if string(w[i+1]) != string(p) {
			t.Fatalf("case %d altered: %x", i, w[i+1])
		}
	}
}
