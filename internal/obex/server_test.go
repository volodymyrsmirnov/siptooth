package obex

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type srv struct {
	pe        *peer
	res       chan error
	connected atomic.Int32
	calls     chan putCall
}

type putCall struct {
	hs   Headers
	body []byte
}

func startServer(t *testing.T, code byte) *srv {
	t.Helper()
	a, b := net.Pipe()
	s := &srv{pe: &peer{t: t, c: NewConn(b, 1<<16)}, res: make(chan error, 1), calls: make(chan putCall, 8)}
	t.Cleanup(func() { s.pe.c.Close() })
	go func() {
		s.res <- Serve(a, testTarget, func(hs Headers, body []byte) byte {
			s.calls <- putCall{hs, body}
			return code
		}, func() { s.connected.Add(1) })
	}()
	return s
}

func (s *srv) connect() {
	s.pe.t.Helper()
	s.pe.send(Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 4096}, Headers: Headers{Bytes(HTarget, testTarget)}})
	if p, err := ParseResponse(s.pe.recvRaw(), true); err != nil || p.Code != RespSuccess {
		s.pe.t.Fatalf("connect: %+v %v", p, err)
	}
}

func (s *srv) expect(code byte) {
	s.pe.t.Helper()
	p, err := ParseResponse(s.pe.recvRaw(), false)
	if err != nil || p.Code != code {
		s.pe.t.Fatalf("got 0x%02X (%v), want 0x%02X", p.Code, err, code)
	}
}

func (s *srv) wait() error {
	s.pe.t.Helper()
	select {
	case err := <-s.res:
		return err
	case <-time.After(2 * time.Second):
		s.pe.t.Fatal("Serve did not return")
		return nil
	}
}

func put(code byte, hs ...Header) Packet {
	return Packet{Code: code, Headers: append(Headers{U32(HConnectionID, 1)}, hs...)}
}

func TestServeConnectResponse(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.pe.send(Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 4096}, Headers: Headers{Bytes(HTarget, testTarget)}})
	want := append([]byte{0xA0, 0x00, 0x1F, 0x10, 0x00, 0x20, 0x00, 0xCB, 0, 0, 0, 1, 0x4A, 0x00, 0x13}, testTarget...)
	if b := s.pe.recvRaw(); !bytes.Equal(b, want) {
		t.Fatalf("got % X", b)
	}
	// onConnect runs after the response is written; with no event it still ran.
	deadline := time.Now().Add(2 * time.Second)
	for s.connected.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Further traffic must not call it again.
	s.pe.send(Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 4096}, Headers: Headers{Bytes(HTarget, testTarget)}})
	s.expect(RespBadRequest)
	if n := s.connected.Load(); n != 1 {
		t.Fatalf("onConnect called %d times", n)
	}
}

func TestServeOnConnectNotCalledOnRefusal(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.pe.send(Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 4096}})
	s.expect(RespForbidden)
	s.pe.send(Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 4096}, Headers: Headers{Bytes(HTarget, []byte("wrong"))}})
	s.expect(RespForbidden)
	if s.connected.Load() != 0 {
		t.Fatal("onConnect called")
	}
	s.connect() // still awaiting-connect
	s.pe.send(Packet{Code: OpDisconnect})
	s.expect(RespSuccess)
	s.wait()
}

func TestServeRequestBeforeConnect(t *testing.T) {
	s := startServer(t, RespSuccess)
	for _, p := range []Packet{{Code: OpPutFinal}, {Code: OpGetFinal}, {Code: OpDisconnect}, {Code: OpAbort}} {
		s.pe.send(p)
		s.expect(RespBadRequest)
	}
	if len(s.calls) != 0 {
		t.Fatal("handler called")
	}
}

func TestServeNilOnConnect(t *testing.T) {
	a, b := net.Pipe()
	pe := &peer{t: t, c: NewConn(b, 1<<16)}
	defer pe.c.Close()
	go Serve(a, testTarget, nil, nil)
	s := &srv{pe: pe}
	s.connect()
	pe.send(put(OpPutFinal))
	s.expect(RespNotImplemented)
}

func TestServeConnectionIDMismatch(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(Packet{Code: OpPutFinal, Headers: Headers{U32(HConnectionID, 9), Bytes(HEndOfBody, []byte("x"))}})
	s.expect(RespForbidden)
	if len(s.calls) != 0 {
		t.Fatal("handler called")
	}
	// A multipart put with a wrong id on a later packet is rejected and the operation ends there.
	s.pe.send(put(OpPut, Bytes(HBody, []byte("a"))))
	s.expect(RespContinue)
	s.pe.send(Packet{Code: OpPut, Headers: Headers{U32(HConnectionID, 9)}})
	s.expect(RespForbidden)
	if len(s.calls) != 0 {
		t.Fatal("handler called")
	}
	// The next Put is a fresh operation (nothing of "a" is carried over).
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("ok"))))
	s.expect(RespSuccess)
	if c := <-s.calls; string(c.body) != "ok" {
		t.Fatalf("body %q", c.body)
	}
}

func TestServeMultipartPut(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(put(OpPut, ASCII(HType, "x-bt/MAP-event-report"), Bytes(HAppParams, EncodeAppParams(AppParam{Tag: 1, Value: []byte{1}}))))
	s.expect(RespContinue)
	if len(s.calls) != 0 {
		t.Fatal("handler called early")
	}
	s.pe.send(put(OpPut, Bytes(HBody, []byte("<a>"))))
	s.expect(RespContinue)
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("</a>"))))
	s.expect(RespSuccess)
	c := <-s.calls
	if string(c.body) != "<a></a>" {
		t.Fatalf("body %q", c.body)
	}
	if v, _ := c.hs.Get(HType); DecodeASCII(v) != "x-bt/MAP-event-report" {
		t.Fatal("type lost")
	}
	if _, ok := c.hs.Get(HAppParams); !ok {
		t.Fatal("app params lost")
	}
	if _, ok := c.hs.Get(HConnectionID); ok {
		t.Fatal("connection id leaked to handler")
	}
}

func TestServeHandlerCode(t *testing.T) {
	s := startServer(t, RespInternalError)
	s.connect()
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("x"))))
	s.expect(RespInternalError)
}

func TestServeBodyCap(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	chunk := bytes.Repeat([]byte{'x'}, 8000)
	sent := 0
	var code byte
	for sent <= MaxBody {
		s.pe.send(put(OpPut, Bytes(HBody, chunk)))
		sent += len(chunk)
		p, _ := ParseResponse(s.pe.recvRaw(), false)
		code = p.Code
	}
	if code != RespEntityTooLarge {
		t.Fatalf("got 0x%02X", code)
	}
	if len(s.calls) != 0 {
		t.Fatal("handler called")
	}
	// The reject ended the operation: the next Put starts afresh.
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("fine"))))
	s.expect(RespSuccess)
}

func TestServeRejectedNonFinalResetsPut(t *testing.T) {
	setTimeouts(t, 30*time.Millisecond, 60*time.Millisecond, time.Second)
	s := startServer(t, RespSuccess)
	s.connect()
	chunk := bytes.Repeat([]byte{'x'}, 8000)
	for sent, code := 0, byte(0); code != RespEntityTooLarge; sent += len(chunk) {
		if sent > MaxBody+len(chunk) {
			t.Fatal("never rejected")
		}
		s.pe.send(put(OpPut, Bytes(HBody, chunk)))
		p, _ := ParseResponse(s.pe.recvRaw(), false)
		code = p.Code
	}
	// Idle after the reject: no put deadline may close the connection.
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-s.res:
		t.Fatalf("Serve returned while idle: %v", err)
	default:
	}
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("fine"))))
	s.expect(RespSuccess)
	if len(s.calls) != 1 {
		t.Fatal("handler not called once")
	}
}

func TestServeHeaderCap(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	chunk := bytes.Repeat([]byte{'x'}, 8000)
	var code byte
	for i := 0; i < 9; i++ {
		s.pe.send(put(OpPut, Bytes(HAppParams, chunk)))
		p, _ := ParseResponse(s.pe.recvRaw(), false)
		code = p.Code
	}
	if code != RespEntityTooLarge {
		t.Fatalf("got 0x%02X", code)
	}
	s.pe.send(put(OpPutFinal))
	s.expect(RespSuccess) // the reject ended the operation; this is a fresh (empty) one
	if len(s.calls) != 1 {
		t.Fatal("handler not called once")
	}
}

func TestServeAbort(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(put(OpPut, ASCII(HType, "old"), Bytes(HBody, []byte("old"))))
	s.expect(RespContinue)
	s.pe.send(Packet{Code: OpAbort})
	s.expect(RespSuccess)
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("new"))))
	s.expect(RespSuccess)
	c := <-s.calls
	if string(c.body) != "new" {
		t.Fatalf("body %q", c.body)
	}
	if _, ok := c.hs.Get(HType); ok {
		t.Fatal("aborted headers leaked")
	}
}

func TestServeNotImplemented(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(Packet{Code: OpGetFinal})
	s.expect(RespNotImplemented)
	s.pe.send(Packet{Code: 0x85}) // SetPath
	s.expect(RespNotImplemented)
	s.pe.send(Packet{Code: 0x55})
	s.expect(RespNotImplemented)
}

func TestServeDisconnect(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(Packet{Code: OpDisconnect})
	s.expect(RespSuccess)
	if err := s.wait(); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestServeIdleHasNoDeadline(t *testing.T) {
	setTimeouts(t, 20*time.Millisecond, 40*time.Millisecond, time.Second)
	s := startServer(t, RespSuccess)
	s.connect()
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-s.res:
		t.Fatalf("Serve returned while idle: %v", err)
	default:
	}
	s.pe.send(put(OpPutFinal, Bytes(HEndOfBody, []byte("x"))))
	s.expect(RespSuccess)
	// ... and the same before Connect.
	s2 := startServer(t, RespSuccess)
	time.Sleep(100 * time.Millisecond)
	s2.connect()
}

func TestServePutPacketTimeout(t *testing.T) {
	setTimeouts(t, 30*time.Millisecond, time.Minute, time.Second)
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.send(put(OpPut, Bytes(HBody, []byte("a"))))
	s.expect(RespContinue)
	if err := s.wait(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestServePutOperationTimeout(t *testing.T) {
	setTimeouts(t, time.Second, 100*time.Millisecond, time.Second)
	s := startServer(t, RespSuccess)
	s.connect()
	go func() {
		for i := 0; i < 50; i++ {
			s.pe.c.Write(mustMarshal(t, put(OpPut, Bytes(HBody, []byte("a")))))
			if _, err := s.pe.c.Next(time.Second); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := s.wait(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestServeEOF(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.c.Close()
	if err := s.wait(); err != io.EOF && err == nil {
		t.Fatalf("got %v", err)
	}
}

func TestServeProtocolError(t *testing.T) {
	s := startServer(t, RespSuccess)
	s.connect()
	s.pe.c.Write([]byte{0x82, 0x00, 0x08, 0x49, 0x00, 0x09, 'h', 'i'})
	if err := s.wait(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}

func TestClientServer(t *testing.T) {
	a, b := net.Pipe()
	calls := make(chan []byte, 1)
	res := make(chan error, 1)
	conn := make(chan struct{})
	go func() {
		res <- Serve(b, testTarget, func(hs Headers, body []byte) byte { calls <- body; return RespSuccess }, func() { close(conn) })
	}()
	cl := NewClient(a)
	defer cl.Close()
	if err := cl.Connect(t.Context(), testTarget, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	<-conn
	if err := cl.Put(Headers{ASCII(HType, "t")}, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if string(<-calls) != "hello" {
		t.Fatal("body")
	}
	if err := cl.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if err := <-res; err != nil {
		t.Fatal(err)
	}
}
