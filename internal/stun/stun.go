// Package stun is a minimal RFC 5389 Binding client: just enough to learn the
// public mapping of a UDP socket.
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

const (
	magicCookie       = 0x2112A442
	headerLen         = 20
	typeBindingReq    = 0x0001
	typeBindingOK     = 0x0101
	attrMappedAddress = 0x0001
	attrXORMapped     = 0x0020
)

// NewBindingRequest builds a Binding Request with a random transaction ID.
func NewBindingRequest() (txID [12]byte, msg []byte) {
	if _, err := rand.Read(txID[:]); err != nil {
		panic("stun: crypto/rand failed: " + err.Error())
	}
	msg = make([]byte, headerLen)
	binary.BigEndian.PutUint16(msg[0:2], typeBindingReq)
	binary.BigEndian.PutUint32(msg[4:8], magicCookie)
	copy(msg[8:20], txID[:])
	return txID, msg
}

// IsSTUN reports whether b looks like a STUN message (RFC 7983 demux): the top
// two bits are zero and the magic cookie is at bytes 4..8.
func IsSTUN(b []byte) bool {
	return len(b) >= headerLen && b[0]&0xC0 == 0 && binary.BigEndian.Uint32(b[4:8]) == magicCookie
}

// ParseBindingResponse extracts the mapped address from a Binding success
// response for txID. XOR-MAPPED-ADDRESS wins; MAPPED-ADDRESS is the fallback.
func ParseBindingResponse(b []byte, txID [12]byte) (netip.AddrPort, error) {
	if !IsSTUN(b) {
		return netip.AddrPort{}, errors.New("stun: not a STUN message")
	}
	if t := binary.BigEndian.Uint16(b[0:2]); t != typeBindingOK {
		return netip.AddrPort{}, fmt.Errorf("stun: unexpected message type 0x%04x", t)
	}
	if [12]byte(b[8:20]) != txID {
		return netip.AddrPort{}, errors.New("stun: transaction ID mismatch")
	}
	total := headerLen + int(binary.BigEndian.Uint16(b[2:4]))
	if total > len(b) {
		return netip.AddrPort{}, errors.New("stun: message length beyond buffer")
	}
	attrs := b[headerLen:total]
	var mapped netip.AddrPort
	var haveMapped bool
	for len(attrs) > 0 {
		if len(attrs) < 4 {
			return netip.AddrPort{}, errors.New("stun: truncated attribute header")
		}
		typ := binary.BigEndian.Uint16(attrs[0:2])
		l := int(binary.BigEndian.Uint16(attrs[2:4]))
		if 4+l > len(attrs) {
			return netip.AddrPort{}, errors.New("stun: attribute length beyond buffer")
		}
		val := attrs[4 : 4+l]
		switch typ {
		case attrXORMapped:
			return parseAddress(val, true, txID)
		case attrMappedAddress:
			if ap, err := parseAddress(val, false, txID); err == nil && !haveMapped {
				mapped, haveMapped = ap, true
			}
		}
		adv := 4 + (l+3)&^3
		if adv > len(attrs) {
			adv = len(attrs) // final attribute may lack padding
		}
		attrs = attrs[adv:]
	}
	if haveMapped {
		return mapped, nil
	}
	return netip.AddrPort{}, errors.New("stun: no mapped address attribute")
}

func parseAddress(v []byte, xor bool, txID [12]byte) (netip.AddrPort, error) {
	if len(v) < 4 {
		return netip.AddrPort{}, errors.New("stun: address attribute too short")
	}
	port := binary.BigEndian.Uint16(v[2:4])
	var mask [16]byte
	binary.BigEndian.PutUint32(mask[0:4], magicCookie)
	copy(mask[4:], txID[:])
	if xor {
		port ^= magicCookie >> 16
	}
	var addr netip.Addr
	switch v[1] {
	case 1:
		if len(v) < 8 {
			return netip.AddrPort{}, errors.New("stun: IPv4 address truncated")
		}
		ip := [4]byte(v[4:8])
		if xor {
			for i := range ip {
				ip[i] ^= mask[i]
			}
		}
		addr = netip.AddrFrom4(ip)
	case 2:
		if len(v) < 20 {
			return netip.AddrPort{}, errors.New("stun: IPv6 address truncated")
		}
		ip := [16]byte(v[4:20])
		if xor {
			for i := range ip {
				ip[i] ^= mask[i]
			}
		}
		addr = netip.AddrFrom16(ip)
	default:
		return netip.AddrPort{}, fmt.Errorf("stun: unknown address family %d", v[1])
	}
	if addr.IsUnspecified() || port == 0 {
		return netip.AddrPort{}, errors.New("stun: mapped address is unspecified or has port 0")
	}
	return netip.AddrPortFrom(addr, port), nil
}

// Query sends a Binding Request from conn to server and returns the mapped
// address. Each attempt waits up to timeout. Non-matching packets are skipped.
// Only use it on a socket nobody else reads (before RTP reading starts, or a
// standalone socket such as doctor's): it uses read deadlines on conn.
func Query(conn net.PacketConn, server string, timeout time.Duration, attempts int) (netip.AddrPort, error) {
	dst, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("stun: resolve %s: %w", server, err)
	}
	if attempts < 1 {
		attempts = 1
	}
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 1500)
	lastErr := errors.New("stun: no response")
	for i := 0; i < attempts; i++ {
		txID, msg := NewBindingRequest()
		if _, err := conn.WriteTo(msg, dst); err != nil {
			return netip.AddrPort{}, fmt.Errorf("stun: send to %s: %w", server, err)
		}
		deadline := time.Now().Add(timeout)
		if err := conn.SetReadDeadline(deadline); err != nil {
			return netip.AddrPort{}, err
		}
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					lastErr = fmt.Errorf("stun: %s: timeout", server)
					break
				}
				return netip.AddrPort{}, fmt.Errorf("stun: read: %w", err)
			}
			if ap, perr := ParseBindingResponse(buf[:n], txID); perr == nil {
				return ap, nil
			}
		}
	}
	return netip.AddrPort{}, lastErr
}
