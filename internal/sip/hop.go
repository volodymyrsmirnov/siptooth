package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"
)

// pinnedHop resolves hop ("host:port") once and returns the same address
// until forgetHop drops it. OPTIONS, REGISTER, INVITE and the keepalives all
// go through it, so they share one flow even when the name has several
// addresses (spec 5.5).
func (u *UA) pinnedHop(ctx context.Context, hop string) (netip.AddrPort, error) {
	u.mu.Lock()
	ap, ok := u.hops[hop]
	u.mu.Unlock()
	if ok {
		return ap, nil
	}
	host, portStr, err := net.SplitHostPort(hop)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ap = netip.AddrPortFrom(ip.Unmap().WithZone(""), uint16(port))
	} else {
		ips, err := u.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("sip: resolve %s: %w", host, err)
		}
		ip := preferIPv4(ips)
		if !ip.IsValid() {
			return netip.AddrPort{}, errors.New("sip: " + host + " has no addresses")
		}
		ap = netip.AddrPortFrom(ip, uint16(port))
	}
	u.mu.Lock()
	if prev, ok := u.hops[hop]; ok { // lost a race: keep the first answer
		ap = prev
	} else if !u.closed {
		u.hops[hop] = ap
	}
	u.mu.Unlock()
	return ap, nil
}

// forgetHop drops the pinned address of hop: the connection or flow to it is
// broken, so the next use resolves again.
func (u *UA) forgetHop(hop string) {
	u.mu.Lock()
	delete(u.hops, hop)
	u.mu.Unlock()
}

// destination is what goes into a request's destination for hop on the given
// transport: the pinned "ip:port". Over TLS a host name stays a name, because
// sipgo derives the server name to verify from the destination host; the
// name is then resolved by sipgo per connection.
func (u *UA) destination(ctx context.Context, transport, hop string) (string, error) {
	if strings.EqualFold(transport, "tls") {
		if host, _, err := net.SplitHostPort(hop); err == nil {
			if _, perr := netip.ParseAddr(host); perr != nil {
				return hop, nil
			}
		}
	}
	ap, err := u.pinnedHop(ctx, hop)
	if err != nil {
		return "", err
	}
	return ap.String(), nil
}

// writeRequest sends req (an ACK, which has no transaction) straight to the
// transport. Unlike sipgo's Client.WriteRequest the whole path is bounded by
// ctx: DNS, connect and TLS handshake as well as the write (which has the
// transport's own write timeout). A TLS handshake that fails for another
// reason than ctx leaves its TCP socket to the garbage collector; that is
// sipgo's TLS dial, which cannot be wrapped from outside.
func (u *UA) writeRequest(ctx context.Context, req *gosip.Request) error {
	if req.Via() == nil {
		if err := sipgo.ClientRequestAddVia(u.client, req); err != nil {
			return err
		}
	}
	if req.Body() == nil {
		req.SetBody(nil) // Content-Length
	}
	conn, err := u.ua.TransportLayer().ClientRequestConnection(ctx, req)
	if err != nil {
		return err
	}
	defer conn.TryClose()
	return conn.WriteMsg(req)
}
