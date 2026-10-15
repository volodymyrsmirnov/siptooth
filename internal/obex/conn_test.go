package obex

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type chunkReader struct {
	data  []byte
	chunk int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.chunk, len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
func (r *chunkReader) Write(p []byte) (int, error) { return len(p), nil }
func (r *chunkReader) Close() error                { return nil }

var pktA = []byte{0x82, 0x00, 0x08, 0x49, 0x00, 0x05, 'h', 'i'}
var pktB = []byte{0xA0, 0x00, 0x03}

func TestConnChunking(t *testing.T) {
	for name, tc := range map[string]struct {
		chunk int
		data  []byte
	}{
		"1-byte chunks":    {1, append(append([]byte(nil), pktA...), pktB...)},
		"two in one read":  {4096, append(append([]byte(nil), pktA...), pktB...)},
		"split mid-packet": {5, append(append([]byte(nil), pktA...), pktB...)},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewConn(&chunkReader{data: tc.data, chunk: tc.chunk}, MaxPacket)
			for _, want := range [][]byte{pktA, pktB} {
				got, err := c.Next(time.Second)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("got % X, %v", got, err)
				}
			}
			if _, err := c.Next(time.Second); err != io.EOF {
				t.Fatalf("got %v", err)
			}
			<-c.Done()
			if c.Err() != io.EOF {
				t.Fatalf("Err = %v", c.Err())
			}
		})
	}
}

func TestConnBadLength(t *testing.T) {
	for name, data := range map[string][]byte{
		"below 3":   {0x82, 0x00, 0x02},
		"above max": {0x82, 0x20, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewConn(&chunkReader{data: data, chunk: 1}, 8192)
			if _, err := c.Next(time.Second); !errors.Is(err, ErrProtocol) {
				t.Fatalf("got %v", err)
			}
			select {
			case <-c.Done():
			default:
				t.Fatal("Done not closed")
			}
		})
	}
}

func TestConnTruncatedEOF(t *testing.T) {
	c := NewConn(&chunkReader{data: pktA[:5], chunk: 5}, MaxPacket)
	if _, err := c.Next(time.Second); err != io.ErrUnexpectedEOF {
		t.Fatalf("got %v", err)
	}
}

func TestConnNextTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, MaxPacket)
	defer c.Close()
	if _, err := c.Next(20 * time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestConnIdleEOF(t *testing.T) {
	a, b := net.Pipe()
	c := NewConn(a, MaxPacket)
	b.Close()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed on idle EOF")
	}
	if c.Err() != io.EOF {
		t.Fatalf("Err = %v", c.Err())
	}
}

func TestConnCloseUnblocksPendingPacket(t *testing.T) {
	a, b := net.Pipe()
	c := NewConn(a, MaxPacket)
	go b.Write(pktA)
	time.Sleep(20 * time.Millisecond) // reader is now blocked handing over the packet
	c.Close()
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop")
	}
	c.Close() // twice is fine
}

type slowWriter struct {
	got  []byte
	zero bool
}

func (w *slowWriter) Read([]byte) (int, error) { select {} }
func (w *slowWriter) Write(p []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	w.got = append(w.got, p[0])
	return 1, nil
}
func (w *slowWriter) Close() error { return nil }

func TestConnWriteLoops(t *testing.T) {
	w := &slowWriter{}
	c := NewConn(w, MaxPacket)
	if err := c.Write(pktA); err != nil || !bytes.Equal(w.got, pktA) {
		t.Fatalf("got % X, %v", w.got, err)
	}
	w.zero = true
	if err := c.Write(pktA); err != io.ErrShortWrite {
		t.Fatalf("got %v", err)
	}
}

func TestConnWriteDeadline(t *testing.T) {
	old := packetTimeout
	packetTimeout = 30 * time.Millisecond
	defer func() { packetTimeout = old }()
	a, b := net.Pipe()
	defer b.Close()
	c := NewConn(a, MaxPacket)
	defer c.Close()
	if err := c.Write(pktA); err == nil {
		t.Fatal("want deadline error")
	}
}
