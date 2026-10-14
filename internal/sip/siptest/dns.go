package siptest

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// NewResolver starts a tiny DNS server on loopback that answers A queries for
// the given names (other names get NXDOMAIN, AAAA queries get an empty
// answer) and returns a net.Resolver that talks only to it. Pass it as
// Options.Resolver to prove per-destination behaviour (such as the TLS
// ServerName) without depending on the machine's DNS.
func NewResolver(t testing.TB, hosts map[string]netip.Addr) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("siptest: DNS listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })

	table := make(map[string]netip.Addr, len(hosts))
	for h, a := range hosts {
		table[strings.ToLower(strings.TrimSuffix(h, "."))] = a
	}
	go serveDNS(pc, table)

	server := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp4", server)
		},
	}
}

func serveDNS(pc net.PacketConn, table map[string]netip.Addr) {
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := dnsAnswer(buf[:n], table); resp != nil {
			pc.WriteTo(resp, from)
		}
	}
}

// dnsAnswer builds the response to a single-question query.
func dnsAnswer(q []byte, table map[string]netip.Addr) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	// QNAME
	i := 12
	var labels []string
	for {
		if i >= len(q) {
			return nil
		}
		l := int(q[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 || i+1+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += 1 + l
	}
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i : i+2])
	question := q[12 : i+4]
	name := strings.ToLower(strings.Join(labels, "."))

	addr, known := table[name]
	rcode := byte(0)
	answers := 0
	var rdata []byte
	if !known {
		rcode = 3 // NXDOMAIN
	} else if qtype == 1 && addr.Is4() {
		a := addr.As4()
		rdata = a[:]
		answers = 1
	}

	resp := make([]byte, 12, 12+len(question)+16)
	copy(resp[0:2], q[0:2])
	resp[2] = 0x81 // QR, RD
	resp[3] = 0x80 | rcode
	binary.BigEndian.PutUint16(resp[4:6], 1)
	binary.BigEndian.PutUint16(resp[6:8], uint16(answers))
	resp = append(resp, question...)
	if answers == 1 {
		resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		resp = append(resp, rdata...)
	}
	return resp
}
