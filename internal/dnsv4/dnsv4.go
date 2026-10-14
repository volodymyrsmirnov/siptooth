// Package dnsv4 provides a DNS resolver that never asks for AAAA records.
//
// Siptooth's media path is IPv4-only (spec 6.4/7), so IPv6 answers are of no
// use. Resolver wraps the connections of Go's pure resolver: an outgoing AAAA
// query is not sent upstream, the next read returns a synthesized
// NOERROR/NODATA reply instead, and every other message passes through
// unchanged. This keeps lookups independent of how a local DNS proxy (for
// example Tailscale MagicDNS) treats AAAA queries.
package dnsv4

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
)

const typeAAAA = 28

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
		return c.Conn.Write(b)
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
