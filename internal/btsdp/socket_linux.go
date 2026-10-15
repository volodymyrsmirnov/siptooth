//go:build linux

package btsdp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrUnsupported is returned by the socket functions on platforms without Bluetooth sockets.
var ErrUnsupported = errors.New("btsdp: bluetooth sockets are only supported on linux")

const (
	solBluetooth = 274
	btSecurity   = 4

	sdpPSM          = 1
	exchangeTimeout = 5 * time.Second
	connectBudget   = 15 * time.Second
	recvBuf         = 64 << 10 // far above any L2CAP MTU: a SEQPACKET read must hold a whole PDU
)

// connectCtx performs a non-blocking connect and waits for it to finish, returning at once when
// ctx ends. An eventfd woken by a ctx watcher interrupts the poll.
func connectCtx(ctx context.Context, fd int, sa unix.Sockaddr) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := unix.Connect(fd, sa)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, unix.EINPROGRESS) && !errors.Is(err, unix.EINTR):
		return err
	}
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return err
	}
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			unix.Write(efd, []byte{1, 0, 0, 0, 0, 0, 0, 0})
		case <-stopped:
		}
	}()
	defer func() {
		close(stopped)
		<-finished
		unix.Close(efd)
	}()
	for {
		fds := []unix.PollFd{
			{Fd: int32(fd), Events: unix.POLLOUT},
			{Fd: int32(efd), Events: unix.POLLIN},
		}
		_, perr := unix.Poll(fds, -1)
		if perr != nil {
			if errors.Is(perr, unix.EINTR) {
				continue
			}
			return perr
		}
		if fds[1].Revents != 0 {
			return ctx.Err()
		}
		if fds[0].Revents != 0 {
			break
		}
	}
	soerr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return err
	}
	if soerr != 0 {
		return unix.Errno(soerr)
	}
	return nil
}

type sdpConn struct{ f *os.File }

// DialSDP opens an L2CAP SEQPACKET connection to the phone's SDP server (PSM 1), bound to the
// adapter address. The returned Transport and Closer are the same connection.
func DialSDP(ctx context.Context, adapter, phone string) (Transport, io.Closer, error) {
	local, err := L2Addr(adapter)
	if err != nil {
		return nil, nil, err
	}
	remote, err := L2Addr(phone)
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_L2CAP)
	if err != nil {
		return nil, nil, fmt.Errorf("btsdp: l2cap socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrL2{PSM: 0, Addr: local}); err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("btsdp: l2cap bind: %w", err)
	}
	if err := connectCtx(ctx, fd, &unix.SockaddrL2{PSM: sdpPSM, Addr: remote}); err != nil {
		unix.Close(fd)
		return nil, nil, fmt.Errorf("btsdp: sdp connect: %w", err)
	}
	// The fd stays non-blocking, so os.File uses the runtime poller and deadlines work.
	c := &sdpConn{f: os.NewFile(uintptr(fd), "sdp")}
	return c, c, nil
}

func (c *sdpConn) Close() error { return c.f.Close() }

// Exchange sends one request and reads one response PDU, within 5 s and ctx.
func (c *sdpConn) Exchange(ctx context.Context, req []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// ctx ending makes any pending I/O fail at once.
	stop := context.AfterFunc(ctx, func() { c.f.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := c.f.SetDeadline(time.Now().Add(exchangeTimeout)); err != nil {
		return nil, err
	}
	fail := func(err error) ([]byte, error) {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	if _, err := c.f.Write(req); err != nil {
		return fail(fmt.Errorf("btsdp: send: %w", err))
	}
	buf := make([]byte, recvBuf)
	n, err := c.f.Read(buf)
	if err != nil {
		return fail(fmt.Errorf("btsdp: receive: %w", err))
	}
	return buf[:n], nil
}

// DialRFCOMM connects to the phone's RFCOMM channel with encryption required (BT_SECURITY
// medium; the bond already exists) and returns the connection as a non-blocking os.File.
func DialRFCOMM(ctx context.Context, adapter, phone string, channel uint8) (*os.File, error) {
	local, err := RFCOMMAddr(adapter)
	if err != nil {
		return nil, err
	}
	remote, err := RFCOMMAddr(phone)
	if err != nil {
		return nil, err
	}
	if channel == 0 {
		return nil, errors.New("btsdp: rfcomm channel 0")
	}
	ctx, cancel := context.WithTimeout(ctx, connectBudget)
	defer cancel()
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_RFCOMM)
	if err != nil {
		return nil, fmt.Errorf("btsdp: rfcomm socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrRFCOMM{Addr: local, Channel: 0}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("btsdp: rfcomm bind: %w", err)
	}
	// struct bt_security { __u8 level; __u8 key_size; }: level 2 = medium.
	if err := unix.SetsockoptString(fd, solBluetooth, btSecurity, string([]byte{2, 0})); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("btsdp: rfcomm BT_SECURITY: %w", err)
	}
	if err := connectCtx(ctx, fd, &unix.SockaddrRFCOMM{Addr: remote, Channel: channel}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("btsdp: rfcomm connect: %w", err)
	}
	return os.NewFile(uintptr(fd), "rfcomm"), nil
}

// FindSMSMAS looks up the phone's SMS Message Access Server record: DialSDP, Lookup, parse and
// choose, all within one 15 s budget.
func FindSMSMAS(ctx context.Context, adapter, phone string) (MASRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, connectBudget)
	defer cancel()
	t, closer, err := DialSDP(ctx, adapter, phone)
	if err != nil {
		return MASRecord{}, err
	}
	defer closer.Close()
	raw, err := Lookup(ctx, t)
	if err != nil {
		return MASRecord{}, err
	}
	recs, err := ParseMASRecords(raw)
	if err != nil {
		return MASRecord{}, err
	}
	return ChooseSMS(recs)
}
