package dnsv4_test

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// aaaaMode selects how the fake server treats AAAA queries.
type aaaaMode int

const (
	aaaaNXDomain   aaaaMode = iota // RCODE=3, like Tailscale MagicDNS
	aaaaNXDomainAA                 // RCODE=3 with the authoritative bit
	aaaaDrop                       // never answered
)

type fakeDNS struct {
	addr string
	mode aaaaMode

	mu    sync.Mutex
	types []uint16 // question types seen
	a     map[string]net.IP
	aaaa  map[string]net.IP
}

// startFakeDNS runs a UDP DNS server on 127.0.0.1. a maps names to IPv4
// answers; aaaa maps names to IPv6 answers (used only when a name has them).
// Names in neither map get NXDOMAIN for every type. A name present only in
// aaaa gets NOERROR/NODATA for A.
func startFakeDNS(t *testing.T, mode aaaaMode, a, aaaa map[string]net.IP) *fakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDNS{addr: pc.LocalAddr().String(), mode: mode, a: a, aaaa: aaaa}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			if resp := f.answer(q); resp != nil {
				pc.WriteTo(resp, from)
			}
		}
	}()
	return f
}

func (f *fakeDNS) seen() []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint16(nil), f.types...)
}

func (f *fakeDNS) sawType(t uint16) bool {
	for _, x := range f.seen() {
		if x == t {
			return true
		}
	}
	return false
}

func (f *fakeDNS) answer(q []byte) []byte {
	if len(q) < 17 {
		return nil
	}
	i := 12
	name := ""
	for q[i] != 0 {
		l := int(q[i])
		name += string(q[i+1:i+1+l]) + "."
		i += 1 + l
	}
	qend := i + 1 + 4
	qtype := binary.BigEndian.Uint16(q[i+1:])
	f.mu.Lock()
	f.types = append(f.types, qtype)
	f.mu.Unlock()

	resp := append([]byte(nil), q[:qend]...)
	flags := uint16(0x8000) | 0x0080 | (binary.BigEndian.Uint16(q[2:]) & 0x0100)
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	binary.BigEndian.PutUint16(resp[10:], 0)

	var ip net.IP
	known := false
	if v, ok := f.a[name]; ok {
		known = true
		if qtype == 1 {
			ip = v
		}
	}
	if v, ok := f.aaaa[name]; ok {
		known = true
		if qtype == 28 {
			ip = v
		}
	}
	switch {
	case qtype == 28 && known && f.aaaa[name] == nil:
		// A-only name: the MagicDNS behaviour under test.
		switch f.mode {
		case aaaaDrop:
			return nil
		case aaaaNXDomainAA:
			flags |= 0x0400
		}
		flags |= 3
	case !known:
		flags |= 3
	case ip != nil:
		binary.BigEndian.PutUint16(resp[6:], 1)
		resp = append(resp, 0xC0, 0x0C)
		rdata := ip.To4()
		if qtype == 28 {
			rdata = ip.To16()
		}
		resp = binary.BigEndian.AppendUint16(resp, qtype)
		resp = binary.BigEndian.AppendUint16(resp, 1)
		resp = binary.BigEndian.AppendUint32(resp, 60)
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata)))
		resp = append(resp, rdata...)
	}
	binary.BigEndian.PutUint16(resp[2:], flags)
	return resp
}

// dialTo returns a Resolver Dial function that always connects to the fake.
func dialTo(addr string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		d.Timeout = 2 * time.Second
		return d.DialContext(ctx, network, addr)
	}
}
