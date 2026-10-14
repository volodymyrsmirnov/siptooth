//go:build unix

package sco

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// unixListener opens a non-blocking AF_UNIX listening socket wrapped like the SCO listener.
func unixListener(t *testing.T) (*os.File, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sco")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "l.sock")
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(fd), "test-listener"), path
}

func unixAccept(fd int) (int, error) {
	nfd, _, err := unix.Accept(fd)
	return nfd, err
}

func TestAcceptFileConnects(t *testing.T) {
	f, path := unixListener(t)
	defer f.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		c, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.Connect(c, &unix.SockaddrUnix{Name: path})
			time.Sleep(200 * time.Millisecond)
			unix.Close(c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	nfd, err := acceptFile(ctx, f, unixAccept)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(nfd)
}

func TestAcceptFileCloseUnblocks(t *testing.T) {
	f, _ := unixListener(t)
	errc := make(chan error, 1)
	go func() {
		_, err := acceptFile(context.Background(), f, unixAccept)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	f.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("want os.ErrClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a pending accept")
	}
	if _, err := acceptFile(context.Background(), f, unixAccept); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("accept after close: want os.ErrClosed, got %v", err)
	}
}

func TestAcceptFileContextCancel(t *testing.T) {
	f, path := unixListener(t)
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := acceptFile(ctx, f, unixAccept)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not unblock a pending accept")
	}
	// The cancellation deadline must not leak into the next Accept.
	go func() {
		time.Sleep(50 * time.Millisecond)
		c, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.Connect(c, &unix.SockaddrUnix{Name: path})
			time.Sleep(200 * time.Millisecond)
			unix.Close(c)
		}
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	nfd, err := acceptFile(ctx2, f, unixAccept)
	if err != nil {
		t.Fatalf("accept after a cancelled accept: %v", err)
	}
	unix.Close(nfd)
}
