//go:build linux

package sco

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	solBluetooth  = 274
	btVoice       = 11
	btVoiceCVSD16 = 0x0060
	solSCO        = 17
	scoOptions    = 1
)

func newSocket() (int, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, unix.BTPROTO_SCO)
	if err != nil {
		return -1, fmt.Errorf("sco socket: %w", err)
	}
	return fd, nil
}

func rawBind(fd int, a [6]byte) error {
	sa := packSockaddr(a)
	if _, _, e := unix.Syscall(unix.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa))); e != 0 {
		return fmt.Errorf("sco bind: %w", e)
	}
	return nil
}

func rawConnect(fd int, a [6]byte) error {
	sa := packSockaddr(a)
	if _, _, e := unix.Syscall(unix.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa))); e != 0 {
		return fmt.Errorf("sco connect: %w", e)
	}
	return nil
}

func rawAccept(fd int) (int, [6]byte, error) {
	var sa [8]byte
	l := uint32(len(sa))
	nfd, _, e := unix.Syscall6(unix.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(&sa[0])), uintptr(unsafe.Pointer(&l)), uintptr(unix.SOCK_CLOEXEC), 0, 0)
	if e != 0 {
		return -1, [6]byte{}, e
	}
	var a [6]byte
	for i := 0; i < 6; i++ {
		a[i] = sa[2+5-i]
	}
	return int(nfd), a, nil
}

func readMTU(fd int) (int, error) {
	var opts [2]byte
	l := uint32(len(opts))
	if _, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), solSCO, scoOptions, uintptr(unsafe.Pointer(&opts[0])), uintptr(unsafe.Pointer(&l)), 0); e != 0 {
		return 0, fmt.Errorf("sco getsockopt SCO_OPTIONS: %w", e)
	}
	mtu := int(binary.LittleEndian.Uint16(opts[:]))
	if mtu <= 0 {
		mtu = 60
	}
	return mtu, nil
}

func setVoiceCVSD(fd int) error {
	var v [2]byte
	binary.LittleEndian.PutUint16(v[:], btVoiceCVSD16)
	if err := unix.SetsockoptString(fd, solBluetooth, btVoice, string(v[:])); err != nil {
		return fmt.Errorf("sco setsockopt BT_VOICE: %w", err)
	}
	return nil
}

type link struct {
	f      *os.File
	mtu    int
	remote string
}

func newLink(fd int, remote [6]byte) (*link, error) {
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	mtu, err := readMTU(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	// os.NewFile on a non-blocking fd registers it with the runtime poller, so Read/Write
	// are cancellable by Close and support deadlines.
	f := os.NewFile(uintptr(fd), "sco")
	return &link{f: f, mtu: mtu, remote: FormatAddr(remote)}, nil
}

// Drain reads queued packets with non-blocking recv calls until the queue is empty (EAGAIN).
// It goes through the raw conn instead of a past read deadline: the runtime poller refuses a
// read outright once the deadline has passed, without trying the syscall.
func (l *link) Drain() (int, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return 0, err
	}
	buf := make([]byte, l.mtu+16)
	n := 0
	var rerr error
	err = rc.Read(func(fd uintptr) bool {
		for n < maxDrainFrames {
			k, e := unix.Read(int(fd), buf)
			if e == unix.EINTR {
				continue
			}
			if e != nil {
				if e != unix.EAGAIN {
					rerr = e
				}
				return true
			}
			if k == 0 { // orderly shutdown: leave it for the next Read to report
				return true
			}
			n++
		}
		return true // never wait for readiness
	})
	if err != nil {
		return n, err
	}
	return n, rerr
}

func (l *link) Read(b []byte) (int, error)  { return l.f.Read(b) }
func (l *link) Write(b []byte) (int, error) { return l.f.Write(b) }
func (l *link) Close() error                { return l.f.Close() }
func (l *link) MTU() int                    { return l.mtu }
func (l *link) Remote() string              { return l.remote }

type listener struct{ f *os.File }

// Listen binds a listening SCO socket to the adapter address.
func Listen(adapter string) (Listener, error) {
	a, err := ParseAddr(adapter)
	if err != nil {
		return nil, err
	}
	fd, err := newSocket()
	if err != nil {
		return nil, err
	}
	if err := rawBind(fd, a); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 1); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("sco listen: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("sco listen nonblock: %w", err)
	}
	// os.NewFile on a non-blocking fd registers it with the runtime poller; Accept
	// waits through it, so Close safely unblocks a pending Accept.
	return &listener{f: os.NewFile(uintptr(fd), "sco-listener")}, nil
}

// Accept waits for a connection until one arrives, ctx ends, or the listener is closed
// (then the error wraps os.ErrClosed).
func (l *listener) Accept(ctx context.Context) (Link, error) {
	var remote [6]byte
	nfd, err := acceptFile(ctx, l.f, func(fd int) (int, error) {
		n, r, err := rawAccept(fd)
		remote = r
		return n, err
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return nil, err
		}
		return nil, fmt.Errorf("sco accept: %w", err)
	}
	return newLink(nfd, remote)
}

func (l *listener) Close() error { return l.f.Close() }

type dialer struct{}

func NewDialer() Dialer { return dialer{} }

// Dial initiates a CVSD SCO connection from adapter to remote with a 5 s deadline
// (the kernel's own SCO connect timeout is 40 s, far too long for a ringing call).
func (dialer) Dial(ctx context.Context, adapter, remote string) (Link, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src, err := ParseAddr(adapter)
	if err != nil {
		return nil, err
	}
	dst, err := ParseAddr(remote)
	if err != nil {
		return nil, err
	}
	fd, err := newSocket()
	if err != nil {
		return nil, err
	}
	if err := rawBind(fd, src); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := setVoiceCVSD(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := rawConnect(fd, dst); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		unix.Close(fd)
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if time.Now().After(deadline) {
			unix.Close(fd)
			return nil, errors.New("sco connect: timeout")
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(fds, 200)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			unix.Close(fd)
			return nil, err
		}
		if n <= 0 || fds[0].Revents&(unix.POLLOUT|unix.POLLERR|unix.POLLHUP) == 0 {
			continue
		}
		soErr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			unix.Close(fd)
			return nil, err
		}
		if soErr != 0 {
			unix.Close(fd)
			return nil, fmt.Errorf("sco connect: %w", unix.Errno(soErr))
		}
		return newLink(fd, dst)
	}
}
