package sip

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestKeepaliveSendsCRLF(t *testing.T) {
	for _, transport := range []string{"tcp", "tls", "udp"} {
		t.Run(transport, func(t *testing.T) {
			tr := siptest.NewTrunk(t, transport)
			u := newTestUA(t, tr, nil)
			// Open the connection/flow the keepalive will ride on.
			u.DiscoverContact(context.Background(), tr.URI("600")) // result irrelevant: loopback has no received/rport
			waitFor(t, "OPTIONS", time.Second, func() bool { return len(tr.RequestsFor("OPTIONS")) > 0 })
			before := tr.Keepalives()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var failed atomic.Value
			u.StartKeepalive(ctx, 50*time.Millisecond, func(err error) { failed.Store(err) })

			waitFor(t, "two CRLF keepalives", 2*time.Second, func() bool { return tr.Keepalives() >= before+2 })
			if err, _ := failed.Load().(error); err != nil {
				t.Errorf("onFail called: %v", err)
			}

			// Cancelling the context stops the pings.
			cancel()
			time.Sleep(100 * time.Millisecond)
			n := tr.Keepalives()
			time.Sleep(200 * time.Millisecond)
			if tr.Keepalives() != n {
				t.Error("keepalive kept running after its context was cancelled")
			}
		})
	}
}

func TestKeepaliveWithoutConnectionIsSkipped(t *testing.T) {
	tr := siptest.NewTrunk(t, "tcp")
	u := newTestUA(t, tr, nil) // no request sent yet: no connection
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failed atomic.Int32
	u.StartKeepalive(ctx, 30*time.Millisecond, func(error) { failed.Add(1) })
	time.Sleep(200 * time.Millisecond)
	if tr.Keepalives() != 0 || failed.Load() != 0 {
		t.Errorf("keepalives=%d failures=%d, want 0/0 while idle", tr.Keepalives(), failed.Load())
	}
}

func TestKeepaliveLoopReportsFailureOnceAndStops(t *testing.T) {
	var sends atomic.Int32
	boom := errors.New("write: broken pipe")
	var reported []error
	done := make(chan struct{})
	go func() {
		defer close(done)
		keepaliveLoop(context.Background(), 10*time.Millisecond,
			func() error {
				if sends.Add(1) == 3 {
					return boom
				}
				return nil
			},
			func(err error) { reported = append(reported, err) })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after a failure")
	}
	if len(reported) != 1 || !errors.Is(reported[0], boom) {
		t.Errorf("reported = %v, want exactly the write error", reported)
	}
	if sends.Load() != 3 {
		t.Errorf("sends = %d, want 3", sends.Load())
	}
}

func TestWriteKeepaliveErrorsOnClosedConnection(t *testing.T) {
	a, b := net.Pipe()
	b.Close()
	conn := &gosip.TCPConnection{Conn: a}
	if err := writeKeepalive(conn, netip.AddrPort{}); err == nil {
		t.Error("writeKeepalive on a dead connection returned nil")
	}
}
