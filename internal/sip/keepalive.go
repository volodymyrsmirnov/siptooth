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
// be nil) and stops the keepalive. onFail runs on the keepalive goroutine and
// must not call Close synchronously (Close waits for that goroutine).
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
	u.loops.Add(1)
	u.mu.Unlock()

	go u.runKeepalive(ctx, every, onFail)
}

func (u *UA) runKeepalive(ctx context.Context, every time.Duration, onFail func(error)) {
	defer u.loops.Done()
	keepaliveLoop(ctx, every, func() error { return u.sendKeepalive(ctx) }, onFail)
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
	hop, err := u.ResolveNextHop(ctx)
	if err != nil {
		return err
	}
	conn, err := u.ua.TransportLayer().GetConnection(u.transport, hop.String())
	if err != nil || conn == nil {
		return nil // no connection to the next hop: nothing to keep alive
	}
	defer conn.TryClose()
	if err := writeKeepalive(conn, hop); err != nil {
		u.forgetHop(u.opts.NextHop)
		return err
	}
	return nil
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
