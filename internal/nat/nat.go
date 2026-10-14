// Package nat chooses the media address to advertise in SDP (spec section 7).
package nat

import (
	"errors"
	"net"
	"net/netip"
	"sync"
)

// Mode is the nat.mode setting, or the effective mode a Decision used.
type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeLocal    Mode = "local"
	ModeSTUN     Mode = "stun"
	ModePublicIP Mode = "public_ip"
)

// Decision is the outcome of Selector.Decide.
type Decision struct {
	Mode       Mode           // effective mode: local, stun or public_ip
	Local      netip.Addr     // local address the RTP socket is bound to
	Media      netip.AddrPort // address to advertise
	STUNServer string         // server that answered, when Mode is stun and it worked
	STUNFailed bool           // every STUN server failed
	Warning    string
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsPrivate reports whether a is RFC1918, loopback, link-local, CGNAT
// (100.64/10) or IPv6 ULA.
func IsPrivate(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || cgnat.Contains(a)
}

// LocalAddrToward returns the source address the kernel would pick toward dst,
// using a connected UDP socket (no packet is sent).
func LocalAddrToward(dst netip.Addr) (netip.Addr, error) {
	if !dst.IsValid() {
		return netip.Addr{}, errors.New("nat: invalid destination address")
	}
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(dst, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, errors.New("nat: unexpected local address type")
	}
	a, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}, errors.New("nat: no local address")
	}
	return a.Unmap(), nil
}

// Selector picks the media address per spec section 7.2. It is safe for
// concurrent use.
type Selector struct {
	mode      Mode
	servers   []string
	publicIP  netip.Addr
	isPrivate func(netip.Addr) bool // test override

	mu       sync.Mutex
	lastGood string
}

// NewSelector builds a Selector. servers are tried in order, last good first.
func NewSelector(mode Mode, servers []string, publicIP netip.Addr) *Selector {
	return &Selector{
		mode:      mode,
		servers:   append([]string(nil), servers...),
		publicIP:  publicIP,
		isPrivate: IsPrivate,
	}
}

func (s *Selector) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.servers))
	if s.lastGood != "" {
		out = append(out, s.lastGood)
	}
	for _, sv := range s.servers {
		if sv != s.lastGood {
			out = append(out, sv)
		}
	}
	return out
}

func (s *Selector) remember(server string) {
	s.mu.Lock()
	s.lastGood = server
	s.mu.Unlock()
}

// Decide chooses the address to advertise. local is the bound RTP address
// (local IP and port); nextHop is the resolved SIP destination. query sends one
// STUN Binding Request (with the caller's timeout/attempts) to a server.
func (s *Selector) Decide(query func(server string) (netip.AddrPort, error), local netip.AddrPort, nextHop netip.Addr) Decision {
	d := Decision{Local: local.Addr(), Media: local}
	mode := s.mode
	if mode == ModeAuto || mode == "" {
		if s.isPrivate(nextHop) {
			mode = ModeLocal
		} else {
			mode = ModeSTUN
		}
	}
	d.Mode = mode

	switch mode {
	case ModePublicIP:
		if s.publicIP.IsValid() {
			d.Media = netip.AddrPortFrom(s.publicIP, local.Port())
		} else {
			d.Warning = "nat.mode public_ip without nat.public_ip: advertising the local address"
		}
	case ModeSTUN:
		for _, sv := range s.order() {
			if ap, err := query(sv); err == nil && ap.IsValid() {
				s.remember(sv)
				d.STUNServer = sv
				d.Media = ap
				return d
			}
		}
		d.STUNFailed = true
		if s.publicIP.IsValid() {
			d.Media = netip.AddrPortFrom(s.publicIP, local.Port())
			d.Warning = "all STUN servers failed: advertising nat.public_ip"
		} else {
			d.Warning = "all STUN servers failed: advertising the local address"
		}
	}
	return d
}
