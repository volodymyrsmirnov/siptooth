//go:build unix

package sco

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// acceptFile waits for and accepts one connection on the non-blocking listening
// socket f. Readiness is handled by the runtime poller through f.SyscallConn, so
// the fd stays reference-counted by f while accept runs: a concurrent f.Close
// unblocks the wait with an error wrapping os.ErrClosed instead of closing the fd under accept
// (no fd-number reuse race). ctx cancellation sets a past read deadline.
func acceptFile(ctx context.Context, f *os.File, accept func(fd int) (int, error)) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, closedErr(f, err)
	}
	// Clear a deadline left behind by a previous cancelled Accept.
	if err := f.SetReadDeadline(time.Time{}); err != nil {
		return -1, closedErr(f, err)
	}
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = f.SetReadDeadline(time.Unix(1, 0))
	})
	defer func() {
		if !stop() {
			<-fired
		}
	}()
	nfd := -1
	var aerr error
	err = rc.Read(func(fd uintptr) bool {
		for {
			nfd, aerr = accept(int(fd))
			if !errors.Is(aerr, unix.EINTR) {
				break
			}
		}
		return !errors.Is(aerr, unix.EAGAIN)
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			return -1, ctxErr
		}
		return -1, closedErr(f, err)
	}
	if aerr != nil {
		return -1, aerr
	}
	return nfd, nil
}

// closedErr maps the internal poll "use of closed file" error, which raw-conn and
// deadline calls return unwrapped, to an error matching os.ErrClosed (Stat applies
// the os package's mapping).
func closedErr(f *os.File, err error) error {
	if _, serr := f.Stat(); errors.Is(serr, os.ErrClosed) {
		return serr
	}
	return err
}
