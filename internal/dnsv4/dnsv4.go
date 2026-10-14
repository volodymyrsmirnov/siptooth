// Package dnsv4 provides a DNS resolver that is IPv4-only and sends no EDNS0.
//
// Siptooth's media path is IPv4-only (spec 6.4/7), so IPv6 answers are of no
// use. Resolver wraps the connections of Go's pure resolver: an outgoing AAAA
// query is not sent upstream, the next read returns a synthesized
// NOERROR/NODATA reply instead. This keeps lookups independent of how a local
// DNS proxy treats AAAA queries.
//
// Go also attaches an EDNS0 OPT record to every query. Tailscale MagicDNS
// answers such a query with the OPT pseudo-record placed before the answer
// record (ANCOUNT=1, ARCOUNT=1), so Go reads the OPT as the only answer, finds
// no address and reports "no such host". The wrapper therefore removes the OPT
// record from outgoing queries (like glibc, which sends none); servers then
// answer classic DNS, and a truncated UDP answer makes Go retry over TCP.
// Every other message passes through unchanged.
package dnsv4

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
)

const (
	typeAAAA = 28
	typeOPT  = 41
)

// Resolver returns a PreferGo resolver whose connections never send AAAA
// queries upstream. base dials the DNS server; nil means a zero net.Dialer.
func Resolver(base func(ctx context.Context, network, address string) (net.Conn, error)) *net.Resolver {
	if base == nil {
		var d net.Dialer
		base = d.DialContext
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := base(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return WrapConn(c), nil
		},
	}
}

// WrapConn wraps a DNS client connection. A connection that is a
// net.PacketConn is treated as UDP (one message per Write/Read); anything
// else as a TCP stream with 2-byte length prefixes. The wrapper keeps the
// net.PacketConn property because Go's resolver chooses its framing by it.
func WrapConn(c net.Conn) net.Conn {
	if _, ok := c.(net.PacketConn); ok {
		return &packetConn{conn: conn{Conn: c}, pc: c.(net.PacketConn)}
	}
	return &conn{Conn: c, stream: true}
}

type conn struct {
	net.Conn
	stream bool

	mu      sync.Mutex
	pending [][]byte // synthesized replies not yet read
}

type packetConn struct {
	conn
	pc net.PacketConn
}

func (c *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := c.Read(b)
	return n, c.RemoteAddr(), err
}

func (c *packetConn) WriteTo(b []byte, _ net.Addr) (int, error) { return c.Write(b) }

func (c *conn) Write(b []byte) (int, error) {
	msg := b
	if c.stream {
		if len(b) < 2 || int(binary.BigEndian.Uint16(b)) != len(b)-2 {
			return c.Conn.Write(b)
		}
		msg = b[2:]
	}
	reply := aaaaReply(msg)
	if reply == nil {
		stripped := stripOPT(msg)
		if stripped == nil {
			return c.Conn.Write(b)
		}
		out := stripped
		if c.stream {
			out = append(binary.BigEndian.AppendUint16(nil, uint16(len(stripped))), stripped...)
		}
		if _, err := c.Conn.Write(out); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	if c.stream {
		reply = append(binary.BigEndian.AppendUint16(nil, uint16(len(reply))), reply...)
	}
	c.mu.Lock()
	c.pending = append(c.pending, reply)
	c.mu.Unlock()
	return len(b), nil
}

func (c *conn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.pending) > 0 {
		n := copy(b, c.pending[0])
		if c.stream && n < len(c.pending[0]) {
			c.pending[0] = c.pending[0][n:]
		} else {
			c.pending = c.pending[1:]
		}
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(b)
}

// aaaaReply returns a NOERROR/NODATA reply when msg is a single-question
// AAAA query, and nil for anything else (including malformed messages).
func aaaaReply(msg []byte) []byte {
	if len(msg) < 12 {
		return nil
	}
	flags := binary.BigEndian.Uint16(msg[2:])
	if flags&0x8000 != 0 || binary.BigEndian.Uint16(msg[4:]) != 1 {
		return nil // a response, or not exactly one question
	}
	i := 12
	for {
		if i >= len(msg) {
			return nil
		}
		l := int(msg[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 { // compression pointers are not valid in a question
			return nil
		}
		i += 1 + l
	}
	if i+4 > len(msg) || binary.BigEndian.Uint16(msg[i:]) != typeAAAA {
		return nil
	}
	end := i + 4
	reply := make([]byte, end)
	copy(reply, msg[:end])
	// QR=1, opcode and RD copied, RA=1, RCODE=0.
	binary.BigEndian.PutUint16(reply[2:], 0x8000|(flags&0x7900)|0x0080)
	binary.BigEndian.PutUint16(reply[6:], 0)  // answers
	binary.BigEndian.PutUint16(reply[8:], 0)  // authority
	binary.BigEndian.PutUint16(reply[10:], 0) // additional
	return reply
}

// stripOPT returns msg without its EDNS0 OPT record when msg is a query with
// one question and exactly one additional record, that being the OPT. It
// returns nil for anything else (including malformed messages).
func stripOPT(msg []byte) []byte {
	if len(msg) < 12 {
		return nil
	}
	if binary.BigEndian.Uint16(msg[2:])&0x8000 != 0 ||
		binary.BigEndian.Uint16(msg[4:]) != 1 ||
		binary.BigEndian.Uint16(msg[6:]) != 0 ||
		binary.BigEndian.Uint16(msg[8:]) != 0 ||
		binary.BigEndian.Uint16(msg[10:]) != 1 {
		return nil
	}
	i := 12
	for {
		if i >= len(msg) {
			return nil
		}
		l := int(msg[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 {
			return nil
		}
		i += 1 + l
	}
	qend := i + 4
	// OPT RR: root name, type 41, class, TTL, RDLEN, RDATA - to the very end.
	if qend+11 > len(msg) || msg[qend] != 0 || binary.BigEndian.Uint16(msg[qend+1:]) != typeOPT {
		return nil
	}
	if qend+11+int(binary.BigEndian.Uint16(msg[qend+9:])) != len(msg) {
		return nil
	}
	out := append([]byte(nil), msg[:qend]...)
	binary.BigEndian.PutUint16(out[10:], 0)
	return out
}
