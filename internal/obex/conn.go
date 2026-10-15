package obex

import (
	"encoding/binary"
	"io"
	"sync"
	"time"
)

// Conn frames OBEX packets on a byte stream (spec 17.5). One reader goroutine cuts packets
// by their length field and hands them to Next, so an EOF on an idle connection is noticed
// at once.
type Conn struct {
	rwc  io.ReadWriteCloser
	pkts chan []byte
	done chan struct{}
	quit chan struct{}
	err  error // set before done is closed

	wmu       sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// NewConn starts the reader. maxIn is the largest packet accepted.
func NewConn(rwc io.ReadWriteCloser, maxIn int) *Conn {
	c := &Conn{rwc: rwc, pkts: make(chan []byte), done: make(chan struct{}), quit: make(chan struct{})}
	go c.read(maxIn)
	return c
}

func (c *Conn) read(maxIn int) {
	var acc []byte
	buf := make([]byte, 4096)
	var err error
loop:
	for {
		var n int
		n, err = c.rwc.Read(buf)
		acc = append(acc, buf[:n]...)
		for len(acc) >= 3 {
			l := int(binary.BigEndian.Uint16(acc[1:]))
			if l < 3 || l > maxIn {
				err = protocolf("bad packet length")
				break loop
			}
			if len(acc) < l {
				break
			}
			pkt := append([]byte(nil), acc[:l]...)
			acc = acc[l:]
			select {
			case c.pkts <- pkt:
			case <-c.quit:
				err = io.ErrClosedPipe
				break loop
			}
		}
		if err != nil {
			if err == io.EOF && len(acc) > 0 {
				err = io.ErrUnexpectedEOF
			}
			break
		}
	}
	c.err = err
	close(c.done)
}

// Next returns the next packet. timeout <= 0 waits forever. A timeout is an error
// (wrapping ErrTimeout); the caller closes.
func (c *Conn) Next(timeout time.Duration) ([]byte, error) {
	var tc <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		tc = t.C
	}
	select {
	case p := <-c.pkts:
		return p, nil
	case <-c.done:
		return nil, c.err
	case <-tc:
		return nil, ErrTimeout
	}
}

// Write writes the whole packet, one writer at a time, with a write deadline when the
// transport supports it.
func (c *Conn) Write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if d, ok := c.rwc.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(packetTimeout))
	}
	for len(b) > 0 {
		n, err := c.rwc.Write(b)
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Done is closed when the reader stops.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err reports why the reader stopped; valid once Done is closed.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close closes the transport and stops the reader. It is safe to call more than once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.quit)
		c.closeErr = c.rwc.Close()
	})
	return c.closeErr
}
