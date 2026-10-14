package hfp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

type EventKind int

const (
	Connected EventKind = iota + 1
	Ring
	CallerID
	CallActive
	CallEnded
	CallSetupCleared
	Disconnected
)

func (k EventKind) String() string {
	switch k {
	case Connected:
		return "connected"
	case Ring:
		return "ring"
	case CallerID:
		return "caller_id"
	case CallActive:
		return "call_active"
	case CallEnded:
		return "call_ended"
	case CallSetupCleared:
		return "call_setup_cleared"
	case Disconnected:
		return "disconnected"
	}
	return fmt.Sprintf("event(%d)", int(k))
}

type Event struct {
	Kind       EventKind
	Number     string
	NumberType int
	At         time.Time
}

var (
	ErrTimeout  = errors.New("hfp: command timeout")
	ErrResponse = errors.New("hfp: command rejected")
	ErrClosed   = errors.New("hfp: closed")
	// ErrNotSent is a timeout before any byte of the command reached the transport (waiting
	// for the previous transaction, or a write deadline with nothing written). It matches
	// ErrTimeout too; callers that must know whether the AG may have seen the command check it.
	ErrNotSent = fmt.Errorf("%w (command not sent)", ErrTimeout)
)

const hfFeatures = 36 // CLI presentation (bit 2) + enhanced call status (bit 5)
const agThreeWay = 1 << 0

type transaction struct {
	prefix string // response prefix routed to this transaction ("" = finals only)
	lines  []string
	done   chan error
}

// Client is one HFP HF service-level connection.
type Client struct {
	t   io.ReadWriteCloser
	log *slog.Logger

	events chan Event

	cmdSem     chan struct{} // one AT transaction at a time; acquisition honours the deadline
	stMu       sync.Mutex    // protects everything below
	tx         *transaction
	ind        map[string]int // name -> 1-based index
	vals       map[string]int // name -> value
	agFeatures int
	ringing    bool
	waiting    bool // a call set up during another call: ignored until its setup ends
	closed     bool
	closeOnce  sync.Once // closes the transport and fails the pending transaction
	quitOnce   sync.Once // closes quit (user Close)

	// sendMu guards the events channel: senders hold it for reading, the reader
	// goroutine takes it for writing to close the channel, so no send ever
	// happens on a closed channel. quit unblocks senders stuck on a full buffer.
	sendMu   sync.RWMutex
	evClosed bool
	quit     chan struct{}
}

// commandTimeout caps Answer and Hangup regardless of the caller's deadline.
var commandTimeout = 3 * time.Second

var errNoResponse = errors.New("no response line")

func New(t io.ReadWriteCloser, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{t: t, log: logger, events: make(chan Event, 32), cmdSem: make(chan struct{}, 1), quit: make(chan struct{}), ind: map[string]int{}, vals: map[string]int{}}
}

func (c *Client) Events() <-chan Event { return c.events }

// Indicators returns a copy of the current indicator values.
func (c *Client) Indicators() map[string]int {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	out := make(map[string]int, len(c.vals))
	for k, v := range c.vals {
		out[k] = v
	}
	return out
}

// Start launches the reader and performs the service level connection.
func (c *Client) Start(ctx context.Context) error {
	go c.readLoop()
	step := func(cmd, prefix string) ([]string, error) {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return c.command(sctx, cmd, prefix)
	}
	lines, err := step(fmt.Sprintf("AT+BRSF=%d", hfFeatures), "+BRSF:")
	if err != nil {
		return c.slcFail(fmt.Errorf("BRSF: %w", err))
	}
	if len(lines) > 0 {
		c.agFeatures, _ = parseBRSF(lines[0])
	}
	lines, err = step("AT+CIND=?", "+CIND:")
	if err == nil && len(lines) == 0 {
		err = errNoResponse
	}
	if err != nil {
		return c.slcFail(fmt.Errorf("CIND=?: %w", err))
	}
	ind, err := parseCINDList(lines[0])
	if err != nil {
		return c.slcFail(err)
	}
	lines, err = step("AT+CIND?", "+CIND:")
	if err == nil && len(lines) == 0 {
		err = errNoResponse
	}
	if err != nil {
		return c.slcFail(fmt.Errorf("CIND?: %w", err))
	}
	vals, err := parseCINDValues(lines[0])
	if err != nil {
		return c.slcFail(err)
	}
	c.stMu.Lock()
	c.ind = ind
	for name, idx := range ind {
		if idx-1 < len(vals) {
			c.vals[name] = vals[idx-1]
		}
	}
	c.stMu.Unlock()
	if _, err := step("AT+CMER=3,0,0,1", ""); err != nil {
		return c.slcFail(fmt.Errorf("CMER: %w", err))
	}
	if c.agFeatures&agThreeWay != 0 {
		if _, err := step("AT+CHLD=?", "+CHLD:"); err != nil {
			c.log.Warn("hfp: AT+CHLD=? failed, continuing", "err", err)
		}
	}
	if _, err := step("AT+CLIP=1", ""); err != nil {
		c.log.Warn("hfp: AT+CLIP=1 failed, caller ID unavailable", "err", err)
	}
	c.emit(Event{Kind: Connected})
	return nil
}

func (c *Client) slcFail(err error) error {
	c.Close()
	return err
}

// command sends one AT command and waits for the final result code. The context deadline
// covers waiting for the previous transaction, the transport write and the response.
func (c *Client) command(ctx context.Context, cmd, prefix string) ([]string, error) {
	select {
	case c.cmdSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrNotSent
	}
	defer func() { <-c.cmdSem }()
	if ctx.Err() != nil {
		return nil, ErrNotSent
	}
	c.stMu.Lock()
	if c.closed {
		c.stMu.Unlock()
		return nil, ErrClosed
	}
	tx := &transaction{prefix: prefix, done: make(chan error, 1)}
	c.tx = tx
	c.stMu.Unlock()
	c.log.Debug("hfp: >>", "cmd", cmd)
	if err := c.write(ctx, []byte(cmd+"\r")); err != nil {
		c.clearTx()
		if errors.Is(err, ErrTimeout) {
			return nil, err // ErrTimeout, or ErrNotSent when nothing was written
		}
		return nil, fmt.Errorf("write: %w", err)
	}
	select {
	case err := <-tx.done:
		c.clearTx()
		return tx.lines, err
	case <-ctx.Done():
		c.clearTx()
		return nil, ErrTimeout
	}
}

type writeDeadliner interface {
	SetWriteDeadline(t time.Time) error
}

// write sends b within ctx's deadline. A transport with write deadlines (a pollable RFCOMM
// fd, net.Conn) gets one; otherwise the write runs on its own goroutine and, if the deadline
// passes first, the transport is closed so the stuck write cannot outlive the command (the
// connection is unusable at that point anyway). A partial write also closes the transport:
// the AT stream would be corrupt.
func (c *Client) write(ctx context.Context, b []byte) error {
	dl, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		_, err := c.t.Write(b)
		return err
	}
	if d, ok := c.t.(writeDeadliner); ok && d.SetWriteDeadline(dl) == nil {
		defer d.SetWriteDeadline(time.Time{})
		n, err := c.t.Write(b)
		if err != nil && isTimeout(err) {
			if n == 0 {
				return ErrNotSent
			}
			c.log.Warn("hfp: partial AT write at the command deadline; closing the connection")
			c.shutdown()
			return ErrTimeout
		}
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.t.Write(b)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.log.Warn("hfp: AT write stalled past the command deadline; closing the connection")
		c.shutdown()
		return ErrTimeout
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *Client) clearTx() {
	c.stMu.Lock()
	c.tx = nil
	c.stMu.Unlock()
}

func (c *Client) Answer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	_, err := c.command(ctx, "ATA", "")
	return err
}

func (c *Client) Hangup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	_, err := c.command(ctx, "AT+CHUP", "")
	return err
}

// Close closes the transport and unblocks any event send waiting on a full
// buffer; it never waits for the events channel to be drained.
func (c *Client) Close() error {
	c.quitOnce.Do(func() { close(c.quit) })
	return c.shutdown()
}

// shutdown marks the client closed, fails the pending transaction and closes the transport.
func (c *Client) shutdown() error {
	var err error
	c.closeOnce.Do(func() {
		c.stMu.Lock()
		c.closed = true
		tx := c.tx
		c.stMu.Unlock()
		if tx != nil {
			select {
			case tx.done <- ErrClosed:
			default:
			}
		}
		err = c.t.Close()
	})
	return err
}

func (c *Client) readLoop() {
	var buf []byte
	chunk := make([]byte, 1024)
	for {
		n, err := c.t.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			var lines []string
			lines, buf = splitLines(buf)
			for _, l := range lines {
				c.handleLine(l)
			}
		}
		if err != nil {
			c.log.Info("hfp: transport closed", "err", err)
			c.shutdown()
			c.stMu.Lock()
			c.ringing = false
			c.stMu.Unlock()
			c.emit(Event{Kind: Disconnected})
			c.sendMu.Lock()
			c.evClosed = true
			close(c.events)
			c.sendMu.Unlock()
			return
		}
	}
}

func (c *Client) handleLine(line string) {
	c.log.Debug("hfp: <<", "line", line)
	c.stMu.Lock()
	tx := c.tx
	c.stMu.Unlock()
	if tx != nil {
		if isFinal(line) {
			res := ErrResponse
			if line == "OK" {
				res = nil
			}
			// Detach the transaction so a second final cannot reach it, and never
			// block the reader: Close may already have delivered ErrClosed.
			c.stMu.Lock()
			if c.tx == tx {
				c.tx = nil
			}
			c.stMu.Unlock()
			select {
			case tx.done <- res:
			default:
			}
			return
		}
		if tx.prefix != "" && strings.HasPrefix(line, tx.prefix) {
			tx.lines = append(tx.lines, line)
			return
		}
	}
	c.handleUnsolicited(line)
}

func (c *Client) handleUnsolicited(line string) {
	switch {
	case line == "RING":
		c.stMu.Lock()
		if c.vals["call"] == 1 { // call waiting: not a call Siptooth may answer
			c.waiting = true
		}
		first := !c.ringing && !c.waiting
		if !c.waiting {
			c.ringing = true
		}
		c.stMu.Unlock()
		if first {
			c.emit(Event{Kind: Ring})
		}
	case strings.HasPrefix(line, "+CLIP:"):
		num, typ, err := parseCLIP(line)
		if err != nil {
			c.log.Warn("hfp: bad +CLIP", "line", line)
			return
		}
		if num != "" {
			c.emit(Event{Kind: CallerID, Number: num, NumberType: typ})
		}
	case strings.HasPrefix(line, "+CIEV:"):
		idx, val, err := parseCIEV(line)
		if err != nil {
			c.log.Warn("hfp: bad +CIEV", "line", line)
			return
		}
		c.applyIndicator(idx, val)
	case strings.HasPrefix(line, "+BCS:"):
		// Codec negotiation was not advertised; if the AG insists, confirm CVSD.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c.command(ctx, "AT+BCS=1", "")
		}()
	default:
		// +BSIR, +VGS, +VGM, +CCWA, +BVRA and anything else: ignored.
	}
}

func (c *Client) applyIndicator(idx, val int) {
	c.stMu.Lock()
	name := ""
	for n, i := range c.ind {
		if i == idx {
			name = n
			break
		}
	}
	if name == "" {
		c.stMu.Unlock()
		return
	}
	prev := c.vals[name]
	c.vals[name] = val
	var out []Event
	switch name {
	case "callsetup":
		// A call set up while another is up is call waiting on a call the handset owns (or the
		// one Siptooth answered): answering it would send ATA and later AT+CHUP into that call
		// (production review 2026-10-05).
		if val == 1 && c.vals["call"] == 1 {
			c.waiting = true // stays ignored after the other call ends, until its setup ends
		}
		if val == 0 {
			c.waiting = false
		}
		if val == 1 && !c.ringing && !c.waiting {
			c.ringing = true
			out = append(out, Event{Kind: Ring})
		}
		if val == 0 && c.ringing {
			c.ringing = false
			if c.vals["call"] == 0 {
				out = append(out, Event{Kind: CallSetupCleared})
			}
		}
	case "call":
		if prev == 0 && val == 1 {
			c.ringing = false
			out = append(out, Event{Kind: CallActive})
		}
		if prev == 1 && val == 0 {
			c.ringing = false // a RING seen during the call must not suppress the next one
			if _, ok := c.ind["callsetup"]; !ok {
				c.waiting = false // without callsetup nothing else would ever end the latch
			}
			out = append(out, Event{Kind: CallEnded})
		}
	}
	c.stMu.Unlock()
	for _, ev := range out {
		c.emit(ev)
	}
}

// emit delivers an event. Ring and CallerID are dropped when the buffer is
// full; state transitions block (backpressure on the AT stream) until the
// consumer drains or Close is called. It never sends on a closed channel:
// the close in readLoop takes sendMu for writing. stMu is never held here.
func (c *Client) emit(ev Event) {
	ev.At = time.Now()
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if c.evClosed {
		return
	}
	select {
	case c.events <- ev:
		return
	default:
	}
	if ev.Kind == Ring || ev.Kind == CallerID {
		c.log.Warn("hfp: event channel full, dropping", "kind", ev.Kind)
		return
	}
	c.log.Warn("hfp: event channel full, waiting for consumer", "kind", ev.Kind)
	select {
	case c.events <- ev:
	case <-c.quit:
		c.log.Warn("hfp: client closed, dropping event", "kind", ev.Kind)
	}
}
