package hfp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// blockingTransport passes through to the AG until block() is called; then every Write
// blocks until Close, and it offers no write deadline (a wedged RFCOMM link).
type blockingTransport struct {
	io.ReadWriteCloser
	mu      sync.Mutex
	blocked bool
	closed  chan struct{}
	once    sync.Once
}

func (b *blockingTransport) block() { b.mu.Lock(); b.blocked = true; b.mu.Unlock() }

func (b *blockingTransport) Write(p []byte) (int, error) {
	b.mu.Lock()
	blocked := b.blocked
	b.mu.Unlock()
	if blocked {
		<-b.closed
		return 0, io.ErrClosedPipe
	}
	return b.ReadWriteCloser.Write(p)
}

func (b *blockingTransport) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.ReadWriteCloser.Close()
}

func shortCommandTimeout(t *testing.T) {
	old := commandTimeout
	commandTimeout = 100 * time.Millisecond
	t.Cleanup(func() { commandTimeout = old })
}

// F: a transport write that never completes is bounded by the 3 s command deadline.
func TestAnswerDeadlineCoversBlockedWrite(t *testing.T) {
	shortCommandTimeout(t)
	_, conn := newFakeAG(t, defaultCIND)
	tr := &blockingTransport{ReadWriteCloser: conn, closed: make(chan struct{})}
	c := New(tr, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-c.Events()
	tr.block()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Answer(context.Background()) }()
	select {
	case err := <-done:
		if err != ErrTimeout {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Answer blocked in a transport write past its deadline")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Answer took %v", d)
	}
}

// stallingAG stops reading after the SLC, so writes on the net.Pipe block; net.Pipe supports
// write deadlines, which the client must use.
func TestAnswerDeadlineUsesWriteDeadline(t *testing.T) {
	shortCommandTimeout(t)
	agSide, hfSide := net.Pipe()
	defer agSide.Close()
	ag := &fakeAG{t: t, conn: agSide, replies: map[string][]string{}, errors: map[string]bool{}, silent: map[string]bool{}, done: make(chan struct{})}
	ag.replies["AT+BRSF=36"] = []string{"+BRSF: 4095"}
	ag.replies["AT+CIND=?"] = []string{defaultCIND}
	ag.replies["AT+CIND?"] = []string{"+CIND: 1,0,0,5,4,0,0"}
	ag.replies["AT+CMER=3,0,0,1"] = nil
	ag.replies["AT+CHLD=?"] = []string{"+CHLD: (0,1,2,3)"}
	ag.replies["AT+CLIP=1"] = nil
	go ag.serveUntil("AT+CLIP=1") // stops reading once the SLC is complete
	c := New(hfSide, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	// nothing could be written before the deadline: a timeout the AG never saw
	if err := c.Answer(context.Background()); !errors.Is(err, ErrTimeout) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Answer took %v", d)
	}
}

// F: waiting for another transaction to finish counts against the command deadline.
func TestAnswerDeadlineCoversTransactionLock(t *testing.T) {
	shortCommandTimeout(t)
	ag, conn := newFakeAG(t, defaultCIND)
	ag.mu.Lock()
	ag.silent["AT+SLOW"] = true
	ag.mu.Unlock()
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-c.Events()
	held := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		close(held)
		c.command(ctx, "AT+SLOW", "") // holds the transaction for 5 s
	}()
	<-held
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	// the ATA was never written: still a timeout, and distinguishable as not sent
	if err := c.Answer(context.Background()); !errors.Is(err, ErrTimeout) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Answer waited %v for the transaction lock", d)
	}
}

// M-6: a RING during an active call (waiting call) must not suppress the next call's RING.
func TestRingDedupClearedWhenCallEnds(t *testing.T) {
	c, ag := startClient(t, `+CIND: ("service",(0,1)),("call",(0,1))`) // no callsetup indicator
	ag.send("+CIEV: 2,1")
	expectEvent(t, c, CallActive)
	ag.send("RING") // during a call: no Ring (call waiting), but it must not suppress the next one
	expectNoEvent(t, c, 100*time.Millisecond)
	ag.send("+CIEV: 2,0")
	expectEvent(t, c, CallEnded)
	ag.send("RING")
	expectEvent(t, c, Ring)
}
