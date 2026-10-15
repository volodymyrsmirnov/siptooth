package obex

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

var testTarget = []byte{0xBB, 0x58, 0x2B, 0x40, 0x42, 0x0C, 0x11, 0xDB, 0xB0, 0xDE, 0x08, 0x00, 0x20, 0x0C, 0x9A, 0x66}

func setTimeouts(t *testing.T, packet, op, abort time.Duration) {
	t.Helper()
	p, o, a := packetTimeout, operationTimeout, abortWait
	packetTimeout, operationTimeout, abortWait = packet, op, abort
	t.Cleanup(func() { packetTimeout, operationTimeout, abortWait = p, o, a })
}

// peer is the scripted other end of a net.Pipe.
type peer struct {
	t *testing.T
	c *Conn
}

func newPipe(t *testing.T) (*Client, *peer) {
	t.Helper()
	a, b := net.Pipe()
	cl := NewClient(a)
	pe := &peer{t: t, c: NewConn(b, 1<<16)}
	t.Cleanup(func() { cl.Close(); pe.c.Close() })
	return cl, pe
}

func (p *peer) recvRaw() []byte {
	p.t.Helper()
	b, err := p.c.Next(2 * time.Second)
	if err != nil {
		p.t.Fatalf("peer recv: %v", err)
	}
	return b
}

func (p *peer) recv() Packet {
	p.t.Helper()
	b := p.recvRaw()
	pk, err := ParseRequest(b)
	if err != nil {
		p.t.Fatalf("peer parse: %v", err)
	}
	return pk
}

func (p *peer) send(pk Packet) {
	p.t.Helper()
	b, err := pk.Marshal()
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.c.Write(b); err != nil {
		p.t.Fatalf("peer send: %v", err)
	}
}

func (p *peer) sendConnect(max uint16, id uint32) {
	p.t.Helper()
	p.send(Packet{Code: RespSuccess, Connect: &ConnectFields{Version: 0x10, MaxPacket: max}, Headers: Headers{U32(HConnectionID, id), Bytes(HWho, testTarget)}})
}

// connected returns a client that has completed Connect with connection id 5.
func connected(t *testing.T) (*Client, *peer) {
	t.Helper()
	cl, pe := newPipe(t)
	go func() { pe.recv(); pe.sendConnect(MaxPacket, 5) }()
	if err := cl.Connect(context.Background(), testTarget, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	return cl, pe
}

func waitDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed")
	}
}

func TestClientConnect(t *testing.T) {
	cl, pe := newPipe(t)
	ap := EncodeAppParams(AppParam{Tag: 0x29, Value: []byte{0, 0, 0, 3}})
	got := make(chan []byte, 1)
	go func() { got <- pe.recvRaw(); pe.sendConnect(1000, 5) }()
	if err := cl.Connect(context.Background(), testTarget, ap, time.Second); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0x80, 0x00, 0x23, 0x10, 0x00, 0x20, 0x00, 0x46, 0x00, 0x13}, testTarget...)
	want = append(want, 0x4C, 0x00, 0x09, 0x29, 0x04, 0, 0, 0, 3)
	if b := <-got; !bytes.Equal(b, want) {
		t.Fatalf("got % X\nwant % X", b, want)
	}
	if cl.peerMax != 1000 {
		t.Fatalf("peerMax %d", cl.peerMax)
	}
	// The Connection-ID is first in every later request.
	go func() {
		p := pe.recv()
		if v, _ := p.Headers.Get(HConnectionID); p.Code != OpPutFinal || len(p.Headers) != 4 || p.Headers[0].ID != HConnectionID || !bytes.Equal(v, []byte{0, 0, 0, 5}) {
			t.Errorf("bad put: %+v", p)
		}
		pe.send(Packet{Code: RespSuccess})
	}()
	if err := cl.Put(Headers{ASCII(HType, "x"), Unicode(HName, "n")}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestClientConnectNoAppParams(t *testing.T) {
	cl, pe := newPipe(t)
	go func() {
		p := pe.recv()
		if _, ok := p.Headers.Get(HAppParams); ok || len(p.Headers) != 1 {
			t.Errorf("headers %+v", p.Headers)
		}
		pe.sendConnect(MaxPacket, 1)
	}()
	if err := cl.Connect(context.Background(), testTarget, nil, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestClientConnectCancel(t *testing.T) {
	cl, pe := newPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- cl.Connect(ctx, testTarget, nil, 0) }()
	pe.recv() // request is out, no answer will come
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return on cancel")
	}
	waitDone(t, cl.Done())
}

func TestClientConnectCancelBlockedWrite(t *testing.T) {
	cl, _ := newPipe(t) // peer never reads: the write blocks
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- cl.Connect(ctx, testTarget, nil, 0) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return on cancel")
	}
}

func TestClientConnectTimeout(t *testing.T) {
	cl, pe := newPipe(t)
	go pe.recv()
	err := cl.Connect(context.Background(), testTarget, nil, 30*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientConnectRefused(t *testing.T) {
	cl, pe := newPipe(t)
	go func() { pe.recv(); pe.send(Packet{Code: RespForbidden}) }()
	err := cl.Connect(context.Background(), testTarget, nil, time.Second)
	var re *ResponseError
	if !errors.As(err, &re) || re.Code != RespForbidden {
		t.Fatalf("got %v", err)
	}
}

func TestClientConnectBadPeerPacket(t *testing.T) {
	cl, pe := newPipe(t)
	go func() { pe.recv(); pe.sendConnect(100, 1) }()
	if err := cl.Connect(context.Background(), testTarget, nil, time.Second); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientGetContinue(t *testing.T) {
	cl, pe := connected(t)
	go func() {
		p := pe.recv()
		if p.Code != OpGetFinal || p.Headers[0].ID != HConnectionID || len(p.Headers) != 3 {
			t.Errorf("first get %+v", p)
		}
		pe.send(Packet{Code: RespContinue, Headers: Headers{Bytes(HBody, []byte("ab"))}})
		p = pe.recv()
		if p.Code != OpGetFinal || len(p.Headers) != 1 || p.Headers[0].ID != HConnectionID {
			t.Errorf("empty get %+v", p)
		}
		pe.send(Packet{Code: RespContinue, Headers: Headers{Bytes(HBody, []byte("cd"))}})
		pe.recv()
		pe.send(Packet{Code: RespSuccess, Headers: Headers{Bytes(HEndOfBody, []byte("ef"))}})
	}()
	body, err := cl.Get(ASCII(HType, "x"), Unicode(HName, "n"))
	if err != nil || string(body) != "abcdef" {
		t.Fatalf("got %q, %v", body, err)
	}
}

func TestClientGetEmptySuccess(t *testing.T) {
	cl, pe := connected(t)
	go func() { pe.recv(); pe.send(Packet{Code: RespSuccess}) }()
	body, err := cl.Get()
	if err != nil || body == nil || len(body) != 0 {
		t.Fatalf("got %v, %v", body, err)
	}
}

func TestClientGetResponseCodes(t *testing.T) {
	for _, code := range []byte{RespNotFound, RespForbidden, 0xCC, RespServiceUnavailable} {
		cl, pe := connected(t)
		go func() { pe.recv(); pe.send(Packet{Code: code}) }()
		_, err := cl.Get()
		var re *ResponseError
		if !errors.As(err, &re) || re.Code != code {
			t.Fatalf("0x%02X: got %v", code, err)
		}
		select {
		case <-cl.Done():
			t.Fatalf("0x%02X: error response must not close", code)
		default:
		}
	}
}

func TestClientInvalidResponseCode(t *testing.T) {
	cl, pe := connected(t)
	go func() { pe.recv(); pe.send(Packet{Code: 0x10}) }()
	if _, err := cl.Get(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientGetBadPacket(t *testing.T) {
	cl, pe := connected(t)
	go func() { pe.recv(); pe.c.Write([]byte{0xA0, 0x00, 0x08, 0x49, 0x00, 0x09, 'h', 'i'}) }()
	if _, err := cl.Get(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientGetPacketTimeout(t *testing.T) {
	setTimeouts(t, 30*time.Millisecond, time.Minute, time.Second)
	cl, pe := connected(t)
	go pe.recv()
	if _, err := cl.Get(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientGetOperationDeadlineAbort(t *testing.T) {
	setTimeouts(t, time.Second, 100*time.Millisecond, time.Second)
	cl, pe := connected(t)
	aborted := make(chan Packet, 1)
	go func() {
		// Header-only Continue answers forever.
		for {
			p := pe.recv()
			if p.Code == OpAbort {
				aborted <- p
				pe.send(Packet{Code: RespSuccess})
				return
			}
			time.Sleep(10 * time.Millisecond)
			pe.send(Packet{Code: RespContinue, Headers: Headers{U32(0xC3, 1)}})
		}
	}()
	_, err := cl.Get()
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	select {
	case p := <-aborted:
		if p.Headers[0].ID != HConnectionID {
			t.Fatalf("abort %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Abort")
	}
	waitDone(t, cl.Done())
}

func TestClientGetOperationDeadlineSilentPeer(t *testing.T) {
	setTimeouts(t, time.Second, 50*time.Millisecond, 50*time.Millisecond)
	cl, pe := connected(t)
	aborted := make(chan struct{})
	go func() {
		pe.recv()
		if p := pe.recv(); p.Code == OpAbort {
			close(aborted)
		}
	}()
	start := time.Now()
	if _, err := cl.Get(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline not honoured")
	}
	<-aborted
	waitDone(t, cl.Done())
}

func TestClientGetBodyCapAbort(t *testing.T) {
	cl, pe := connected(t)
	aborted := make(chan struct{})
	go func() {
		pe.recv()
		chunk := bytes.Repeat([]byte{'x'}, 8000)
		for {
			pe.send(Packet{Code: RespContinue, Headers: Headers{Bytes(HBody, chunk)}})
			if p := pe.recv(); p.Code == OpAbort {
				close(aborted)
				pe.send(Packet{Code: RespSuccess})
				return
			}
		}
	}()
	if _, err := cl.Get(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	<-aborted
	waitDone(t, cl.Done())
}

func TestClientGetHeaderCapAbort(t *testing.T) {
	cl, pe := connected(t)
	aborted := make(chan struct{})
	go func() {
		pe.recv()
		chunk := bytes.Repeat([]byte{'x'}, 8000)
		for {
			pe.send(Packet{Code: RespContinue, Headers: Headers{Bytes(HAppParams, chunk)}})
			if p := pe.recv(); p.Code == OpAbort {
				close(aborted)
				return // no answer: the client gives up after AbortWait
			}
		}
	}()
	setTimeouts(t, time.Second, time.Minute, 50*time.Millisecond)
	if _, err := cl.Get(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	<-aborted
	waitDone(t, cl.Done())
}

func TestClientPut(t *testing.T) {
	cl, pe := connected(t)
	go func() {
		p := pe.recv()
		if b, _ := p.Headers.Get(HEndOfBody); string(b) != "<x/>" || p.Code != OpPutFinal {
			t.Errorf("put %+v", p)
		}
		pe.send(Packet{Code: RespNotAcceptable})
	}()
	var re *ResponseError
	if err := cl.Put(Headers{ASCII(HType, "t")}, []byte("<x/>")); !errors.As(err, &re) || re.Code != RespNotAcceptable {
		t.Fatalf("got %v", err)
	}
}

func TestClientPutContinueIsProtocolError(t *testing.T) {
	cl, pe := connected(t)
	go func() { pe.recv(); pe.send(Packet{Code: RespContinue}) }()
	if err := cl.Put(nil, []byte("x")); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
	waitDone(t, cl.Done())
}

func TestClientPutTooLargeForPeer(t *testing.T) {
	cl, pe := newPipe(t)
	go func() { pe.recv(); pe.sendConnect(300, 5) }()
	if err := cl.Connect(context.Background(), testTarget, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := cl.Put(nil, make([]byte, 400)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-cl.Done():
		t.Fatal("connection must stay usable")
	default:
	}
}

func TestClientDisconnect(t *testing.T) {
	cl, pe := connected(t)
	go func() {
		if p := pe.recv(); p.Code != OpDisconnect || p.Headers[0].ID != HConnectionID {
			t.Errorf("disconnect %+v", p)
		}
		pe.send(Packet{Code: RespSuccess})
	}()
	if err := cl.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, cl.Done())
}

func TestClientIdleEOF(t *testing.T) {
	cl, pe := connected(t)
	pe.c.Close()
	waitDone(t, cl.Done())
}
