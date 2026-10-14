// Package sco opens Bluetooth SCO audio sockets (spec section 3.4).
package sco

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

var ErrUnsupported = errors.New("sco: not supported on this platform")

// Link is one connected SCO socket. Reads return one packet of MTU bytes; writes must be exactly MTU bytes.
type Link interface {
	io.ReadWriteCloser
	MTU() int
	Remote() string
}

// maxDrainFrames bounds Drain: the kernel receive buffer (about 212 KB) holds far fewer packets.
const maxDrainFrames = 8192

// drainer is implemented by links that can discard queued inbound packets without blocking.
type drainer interface {
	Drain() (int, error)
}

// Drain discards every inbound packet already queued on the link and returns how many were
// dropped. With in-band ringing the phone opens SCO while it rings and the kernel accepts that
// link into the listener backlog, queueing ringtone packets until the receive buffer is full;
// without a drain, read-driven pacing would keep that backlog (about a second) as permanent lag.
// Links that cannot drain report 0.
func Drain(l Link) (int, error) {
	if d, ok := l.(drainer); ok {
		return d.Drain()
	}
	return 0, nil
}

type Listener interface {
	Accept(ctx context.Context) (Link, error)
	Close() error
}

type Dialer interface {
	Dial(ctx context.Context, adapter, remote string) (Link, error)
}

// ParseAddr parses AA:BB:CC:DD:EE:FF (case-insensitive) into big-endian byte order.
func ParseAddr(s string) ([6]byte, error) {
	var a [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return a, fmt.Errorf("bad bluetooth address %q", s)
	}
	for i, p := range parts {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) != 1 {
			return a, fmt.Errorf("bad bluetooth address %q", s)
		}
		a[i] = b[0]
	}
	return a, nil
}

func FormatAddr(a [6]byte) string {
	return strings.ToUpper(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", a[0], a[1], a[2], a[3], a[4], a[5]))
}

// packSockaddr builds struct sockaddr_sco: u16 family (AF_BLUETOOTH=31) + bdaddr_t (little-endian).
func packSockaddr(a [6]byte) [8]byte {
	var sa [8]byte
	sa[0] = 31
	for i := 0; i < 6; i++ {
		sa[2+i] = a[5-i]
	}
	return sa
}

// --- in-memory fake used by tests on every platform ---

type pipeLink struct {
	mtu    int
	remote string
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once
	peer   *pipeLink
}

// NewPipe returns two connected fake links with the given MTU.
func NewPipe(mtu int) (Link, Link) {
	ab := make(chan []byte, 64)
	ba := make(chan []byte, 64)
	a := &pipeLink{mtu: mtu, remote: "11:11:11:11:11:11", in: ba, out: ab, closed: make(chan struct{})}
	b := &pipeLink{mtu: mtu, remote: "22:22:22:22:22:22", in: ab, out: ba, closed: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

func (p *pipeLink) MTU() int       { return p.mtu }
func (p *pipeLink) Remote() string { return p.remote }

func (p *pipeLink) Read(buf []byte) (int, error) {
	select {
	case frame := <-p.in:
		return copy(buf, frame), nil
	case <-p.closed:
		return 0, io.EOF
	case <-p.peer.closed:
		// drain anything already queued before reporting EOF
		select {
		case frame := <-p.in:
			return copy(buf, frame), nil
		default:
			return 0, io.EOF
		}
	}
}

func (p *pipeLink) Write(b []byte) (int, error) {
	if len(b) > p.mtu {
		return 0, errors.New("sco: write exceeds MTU (EINVAL)")
	}
	// Fail deterministically once either side is closed; a select with a free buffer
	// slot would otherwise pick the send at random.
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	case <-p.peer.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	frame := append([]byte(nil), b...)
	select {
	case p.out <- frame:
		return len(b), nil
	case <-p.closed:
		return 0, io.ErrClosedPipe
	case <-p.peer.closed:
		return 0, io.ErrClosedPipe
	}
}

// Drain drops queued frames without blocking.
func (p *pipeLink) Drain() (int, error) {
	n := 0
	for n < maxDrainFrames {
		select {
		case <-p.in:
			n++
		default:
			return n, nil
		}
	}
	return n, nil
}

func (p *pipeLink) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}
