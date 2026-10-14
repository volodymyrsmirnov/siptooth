package hfp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func startClient(t *testing.T, cind string) (*Client, *fakeAG) {
	t.Helper()
	ag, conn := newFakeAG(t, cind)
	c := New(conn, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("SLC: %v", err)
	}
	ev := <-c.Events()
	if ev.Kind != Connected {
		t.Fatalf("first event %v", ev.Kind)
	}
	t.Cleanup(func() { c.Close() })
	return c, ag
}

func expectEvent(t *testing.T, c *Client, kind EventKind) Event {
	t.Helper()
	select {
	case ev := <-c.Events():
		if ev.Kind != kind {
			t.Fatalf("got %v, want %v", ev.Kind, kind)
		}
		return ev
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for %v", kind)
	}
	return Event{}
}

func expectNoEvent(t *testing.T, c *Client, d time.Duration) {
	t.Helper()
	select {
	case ev := <-c.Events():
		t.Fatalf("unexpected event %v", ev.Kind)
	case <-time.After(d):
	}
}

func TestSLCSequence(t *testing.T) {
	_, ag := startClient(t, defaultCIND)
	got := strings.Join(ag.got(), " ")
	want := "AT+BRSF=52 AT+CIND=? AT+CIND? AT+CMER=3,0,0,1 AT+CHLD=? AT+CLIP=1 AT+VGS=15 AT+VGM=15"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSLCWithoutThreeWay(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.replies["AT+BRSF=52"] = []string{"+BRSF: 4094"} // bit0 (three-way) cleared
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, cmd := range ag.got() {
		if cmd == "AT+CHLD=?" {
			t.Fatal("AT+CHLD=? must not be sent when AG lacks three-way calling")
		}
	}
}

func TestIncomingCallFlow(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("+CIEV: 3,1")
	expectEvent(t, c, Ring)
	ag.send("RING")
	ag.send(`+CLIP: "+15550100",145`)
	ev := expectEvent(t, c, CallerID)
	if ev.Number != "+15550100" || ev.NumberType != 145 {
		t.Fatalf("%+v", ev)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Answer(ctx); err != nil {
		t.Fatal(err)
	}
	ag.send("+CIEV: 2,1")
	expectEvent(t, c, CallActive)
	ag.send("+CIEV: 3,0")
	expectNoEvent(t, c, 100*time.Millisecond) // callsetup clearing after answer is not a new event
	if err := c.Hangup(ctx); err != nil {
		t.Fatal(err)
	}
	ag.send("+CIEV: 2,0")
	expectEvent(t, c, CallEnded)
	ind := c.Indicators()
	if ind["call"] != 0 || ind["callsetup"] != 0 {
		t.Fatalf("%v", ind)
	}
}

func TestDuplicateRing(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	for i := 0; i < 5; i++ {
		ag.send("RING")
	}
	ag.send("+CIEV: 3,1")
	expectEvent(t, c, Ring)
	expectNoEvent(t, c, 200*time.Millisecond)
}

func TestCallerGaveUp(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("+CIEV: 3,1")
	expectEvent(t, c, Ring)
	ag.send("+CIEV: 3,0")
	expectEvent(t, c, CallSetupCleared)
	// a new ring cycle produces a new Ring
	ag.send("RING")
	expectEvent(t, c, Ring)
}

func TestSLCIndicatorOrder(t *testing.T) {
	c, ag := startClient(t, `+CIND: ("callsetup",(0,3)),("callheld",(0,2)),("call",(0,1)),("service",(0,1))`)
	ag.send("+CIEV: 1,1") // callsetup is index 1 here
	expectEvent(t, c, Ring)
	ag.send("+CIEV: 3,1") // call is index 3
	expectEvent(t, c, CallActive)
}

func TestAnswerErrorAndTimeout(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.errors["ATA"] = true
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-c.Events()
	if err := c.Answer(context.Background()); err != ErrResponse {
		t.Fatalf("got %v", err)
	}
	ag.silent["AT+CHUP"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := c.Hangup(ctx); err != ErrTimeout {
		t.Fatalf("got %v", err)
	}
	// client still usable afterwards
	ag.send("+CIEV: 3,1")
	expectEvent(t, c, Ring)
}

func TestUnsolicitedDuringTransaction(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	// AG sends RING + CIEV between the ATA echo and OK
	ag.replies["ATA"] = []string{"+CIEV: 3,1"}
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-c.Events()
	if err := c.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, c, Ring)
}

func TestChunkedInput(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.sendRaw("\r\n+CIEV: 3,1\r\n\r\nRI")
	time.Sleep(20 * time.Millisecond)
	ag.sendRaw("NG\r\n\r\n+CLIP: \"+1555")
	time.Sleep(20 * time.Millisecond)
	ag.sendRaw("0100\",145\r\n")
	expectEvent(t, c, Ring)
	ev := expectEvent(t, c, CallerID)
	if ev.Number != "+15550100" {
		t.Fatalf("%+v", ev)
	}
}

func TestDisconnect(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.conn.Close()
	expectEvent(t, c, Disconnected)
	if _, ok := <-c.Events(); ok {
		t.Fatal("events channel should be closed after Disconnected")
	}
	if err := c.Answer(context.Background()); err != ErrClosed {
		t.Fatalf("got %v", err)
	}
}

func TestSLCFailsOnError(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.errors["AT+CMER=3,0,0,1"] = true
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("expected SLC failure")
	}
}

func TestStateEventsSurviveFullBuffer(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("RING")
	for i := 0; i < 40; i++ { // overflow the 32-slot buffer without draining
		ag.send(`+CLIP: "+15550100",145`)
	}
	sent := make(chan struct{})
	go func() { // the reader blocks on CallActive, so these writes must not hold up the test
		ag.send("+CIEV: 2,1")
		ag.send("+CIEV: 2,0")
		close(sent)
	}()
	time.Sleep(50 * time.Millisecond)
	var sawActive, sawEnded bool
	deadline := time.After(2 * time.Second)
	for !sawEnded {
		select {
		case ev := <-c.Events():
			switch ev.Kind {
			case CallActive:
				sawActive = true
			case CallEnded:
				if !sawActive {
					t.Fatal("CallEnded before CallActive")
				}
				sawEnded = true
			}
		case <-deadline:
			t.Fatalf("CallActive=%v CallEnded=%v", sawActive, sawEnded)
		}
	}
	<-sent
}

func TestCloseDoesNotBlockOnFullBuffer(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	for i := 0; i < 40; i++ {
		ag.send(`+CLIP: "+15550100",145`)
	}
	go ag.send("+CIEV: 2,1") // reader blocks delivering CallActive
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	// the reader unblocks and closes the channel
	timeout := time.After(time.Second)
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("events channel not closed after Close")
		}
	}
}

func TestAnswerCappedDespiteLongDeadline(t *testing.T) {
	old := commandTimeout
	commandTimeout = 100 * time.Millisecond
	t.Cleanup(func() { commandTimeout = old })
	ag, conn := newFakeAG(t, defaultCIND)
	ag.silent["ATA"] = true
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := c.Answer(ctx); err != ErrTimeout {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Answer took %v; the command timeout must cap the caller deadline", d)
	}
}

func TestSLCCINDErrorWrapped(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.silent["AT+CIND?"] = true
	c := New(conn, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Start(ctx); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

// Production review 2026-10-05: a waiting call (callsetup=1 or RING while call=1) belongs to a
// call the handset already has, which Siptooth does not own; answering it would send ATA and
// later AT+CHUP into the owner's own call. No Ring is emitted while a call is up.
func TestNoRingWhileACallIsActive(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("+CIEV: 2,1") // the owner answered on the handset
	expectEvent(t, c, CallActive)
	ag.send("+CIEV: 3,1") // call waiting
	ag.send("RING")
	expectNoEvent(t, c, 200*time.Millisecond)
	ag.send("+CIEV: 3,0")
	ag.send("+CIEV: 2,0")
	expectEvent(t, c, CallEnded)
	ag.send("+CIEV: 3,1") // a new incoming call once the line is free
	expectEvent(t, c, Ring)
}

func TestNoRingWhenConnectedDuringACall(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.replies["AT+CIND?"] = []string{"+CIND: 1,1,0,5,4,0,0"} // call already active at SLC time
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectEvent(t, c, Connected)
	ag.send("+CIEV: 3,1")
	expectNoEvent(t, c, 200*time.Millisecond)
}

// The waiting call keeps ringing after the owner's call ends: still not ours to answer, until
// its call setup ends.
func TestWaitingCallStaysUnansweredAfterCallEnds(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("+CIEV: 2,1")
	expectEvent(t, c, CallActive)
	ag.send("+CIEV: 3,1") // call waiting
	ag.send("+CIEV: 2,0") // the owner's call ends; the waiting call still rings
	expectEvent(t, c, CallEnded)
	ag.send("RING")
	expectNoEvent(t, c, 200*time.Millisecond)
	ag.send("+CIEV: 3,0") // the waiting call is gone
	ag.send("+CIEV: 3,1") // a new incoming call
	expectEvent(t, c, Ring)
}

// A command waiting for the slot is cancelled unsent and never reaches the phone.
func TestCommandCancelledWhileWaitingForSlot(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.silent["AT+CHUP"] = true
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-c.Events()
	hctx, hcancel := context.WithCancel(context.Background())
	go c.Hangup(hctx) // holds the slot, waiting for a reply that never comes
	deadline := time.Now().Add(time.Second)
	for {
		ag.mu.Lock()
		n := len(ag.received)
		last := ""
		if n > 0 {
			last = ag.received[n-1]
		}
		ag.mu.Unlock()
		if last == "AT+CHUP" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("AT+CHUP never sent")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- c.Answer(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	err := <-res
	if !errors.Is(err, ErrNotSent) || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want ErrNotSent wrapping context.Canceled", err)
	}
	hcancel()
	time.Sleep(20 * time.Millisecond)
	ag.mu.Lock()
	defer ag.mu.Unlock()
	for _, cmd := range ag.received {
		if cmd == "ATA" {
			t.Fatal("the cancelled ATA reached the phone")
		}
	}
}

// Final fix wave A1: the owner placing a call is not a call to answer, but the coordinator must
// know about it (a CallActive that follows is the owner's, never the ATA's).
func TestOutgoingSetupEvent(t *testing.T) {
	for _, setup := range []string{"2", "3"} {
		c, ag := startClient(t, defaultCIND)
		ag.send("+CIEV: 3," + setup)
		expectEvent(t, c, OutgoingSetup)
		if setup == "2" {
			ag.send("+CIEV: 3,3") // dialing -> alerting: still the same outgoing call
			expectNoEvent(t, c, 100*time.Millisecond)
		}
		ag.send("+CIEV: 3,0")
		expectEvent(t, c, CallSetupCleared) // aborted before it was answered
		ag.send("+CIEV: 3,2")
		expectEvent(t, c, OutgoingSetup) // a second attempt is a new event
		ag.send("+CIEV: 2,1")
		expectEvent(t, c, CallActive)
		ag.send("+CIEV: 3,0")
		expectNoEvent(t, c, 100*time.Millisecond)
	}
}

func TestOutgoingSetupNotEmittedDuringACall(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	ag.send("+CIEV: 2,1")
	expectEvent(t, c, CallActive)
	ag.send("+CIEV: 3,2") // a second call placed from the handset while one is active
	expectNoEvent(t, c, 100*time.Millisecond)
}

// A phone that refuses the gain commands still gets a working connection.
func TestSLCVolumeRefused(t *testing.T) {
	ag, conn := newFakeAG(t, defaultCIND)
	ag.errors["AT+VGS=15"] = true
	ag.errors["AT+VGM=15"] = true
	c := New(conn, slog.Default())
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("SLC failed on refused gain: %v", err)
	}
	defer c.Close()
}

// The phone lowering the speaker gain (its volume buttons, or reporting its
// own level when audio connects) is answered with AT+VGS=15: the caller's
// voice reaches siptooth at the phone's full level.
func TestSpeakerGainRestored(t *testing.T) {
	c, ag := startClient(t, defaultCIND)
	defer c.Close()
	n := len(ag.got())
	ag.send("+VGS: 7")
	waitCmd := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(ag.got()) < want {
			if time.Now().After(deadline) {
				t.Fatalf("commands %v", ag.got())
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitCmd(n + 1)
	if got := ag.got()[n]; got != "AT+VGS=15" {
		t.Fatalf("got %q, want AT+VGS=15", got)
	}
	ag.send("+VGS=9") // the older HSP form
	waitCmd(n + 2)
	ag.send("+VGS: 15")
	ag.send("+VGM: 3") // microphone gain is informational
	time.Sleep(50 * time.Millisecond)
	if got := len(ag.got()); got != n+2 {
		t.Fatalf("unexpected commands %v", ag.got()[n:])
	}
}
