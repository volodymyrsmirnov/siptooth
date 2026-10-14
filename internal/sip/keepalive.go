package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	gosip "github.com/emiago/sipgo/sip"
)

// crlfPing is the RFC 5626 keepalive ping.
var crlfPing = []byte("\r\n\r\n")

// StartKeepalive sends a CRLFCRLF keepalive every interval on the connection
// to the next hop (TCP/TLS) or on the UDP flow to it, to hold the NAT mapping
// (spec 5.5). It returns at once and runs until ctx is cancelled or the UA is
// closed.
//
// With no open connection to the next hop there is nothing to keep alive and
// the tick is skipped. A failed write (including one that hits the 5 s write
// timeout) or a failed next hop lookup is reported once to onFail (which may
// be nil) and stops the keepalive.
func (u *UA) StartKeepalive(ctx context.Context, every time.Duration, onFail func(error)) {
	if every <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		cancel()
		return
	}
	u.cancels = append(u.cancels, cancel)
	u.mu.Unlock()

	go keepaliveLoop(ctx, every, func() error { return u.sendKeepalive(ctx) }, onFail)
}

func keepaliveLoop(ctx context.Context, every time.Duration, send func() error, onFail func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := send(); err != nil {
			if ctx.Err() != nil {
				return
			}
			if onFail != nil {
				onFail(err)
			}
			return
		}
	}
}

func (u *UA) sendKeepalive(ctx context.Context) error {
	hop, err := u.cachedNextHop(ctx)
	if err != nil {
		return err
	}
	conn, err := u.ua.TransportLayer().GetConnection(u.transport, hop.String())
	if err != nil || conn == nil {
		return nil // no connection to the next hop: nothing to keep alive
	}
	defer conn.TryClose()
	return writeKeepalive(conn, hop)
}

func (u *UA) cachedNextHop(ctx context.Context) (netip.AddrPort, error) {
	u.mu.Lock()
	hop, ok := u.nextHop, u.haveNext
	u.mu.Unlock()
	if ok {
		return hop, nil
	}
	hop, err := u.ResolveNextHop(ctx)
	if err != nil {
		return netip.AddrPort{}, err
	}
	u.mu.Lock()
	u.nextHop, u.haveNext = hop, true
	u.mu.Unlock()
	return hop, nil
}

// writeKeepalive writes the CRLF ping on conn; raddr is the UDP destination.
func writeKeepalive(conn gosip.Connection, raddr netip.AddrPort) error {
	switch c := conn.(type) {
	case *gosip.TCPConnection: // also carries TLS; its write deadline is the 5 s write timeout
		n, err := c.Write(crlfPing)
		if err != nil {
			return fmt.Errorf("sip: keepalive write: %w", err)
		}
		if n != len(crlfPing) {
			return errors.New("sip: keepalive short write")
		}
		return nil
	case *gosip.UDPConnection:
		if _, err := c.WriteTo(crlfPing, net.UDPAddrFromAddrPort(raddr)); err != nil {
			return fmt.Errorf("sip: keepalive write: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("sip: keepalive on unsupported connection %T", conn)
	}
}
