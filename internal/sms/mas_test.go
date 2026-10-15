package sms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

// fakeEnd is the scripted other end of a net.Pipe, speaking OBEX framing.
type fakeEnd struct {
	t *testing.T
	c *obex.Conn
}

func newFakeEnd(t *testing.T, rwc io.ReadWriteCloser) *fakeEnd {
	f := &fakeEnd{t: t, c: obex.NewConn(rwc, 1<<16)}
	t.Cleanup(func() { f.c.Close() })
	return f
}

func (f *fakeEnd) recvRaw() []byte {
	f.t.Helper()
	b, err := f.c.Next(2 * time.Second)
	if err != nil {
		f.t.Fatalf("fake recv: %v", err)
	}
	return b
}

func (f *fakeEnd) recv() obex.Packet {
	f.t.Helper()
	p, err := obex.ParseRequest(f.recvRaw())
	if err != nil {
		f.t.Fatalf("fake parse: %v", err)
	}
	return p
}

func (f *fakeEnd) send(p obex.Packet) {
	f.t.Helper()
	b, err := p.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.c.Write(b); err != nil {
		f.t.Fatalf("fake send: %v", err)
	}
}

func (f *fakeEnd) connectOK(connID uint32) {
	f.send(obex.Packet{Code: obex.RespSuccess, Connect: &obex.ConnectFields{Version: 0x10, MaxPacket: obex.MaxPacket},
		Headers: obex.Headers{obex.U32(obex.HConnectionID, connID), obex.Bytes(obex.HWho, MASTarget)}})
}

func mustConnectMAS(t *testing.T) (*MAS, *fakeEnd) {
	t.Helper()
	a, b := net.Pipe()
	f := newFakeEnd(t, b)
	go func() { f.recv(); f.connectOK(7) }()
	m, err := ConnectMAS(context.Background(), a, btsdp.MASRecord{Channel: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, f
}

func TestConnectMASFeatures(t *testing.T) {
	head := []byte{0x80, 0, 0, 0x10, 0x00, 0x20, 0x00, 0x46, 0x00, 0x13}
	head = append(head, MASTarget...)
	withFeat := append(append([]byte(nil), head...), 0x4C, 0x00, 0x09, 0x29, 0x04, 0, 0, 0, 3)
	tests := []struct {
		name string
		rec  btsdp.MASRecord
		want []byte
	}{
		{"attribute absent", btsdp.MASRecord{Channel: 5}, head},
		{"present without bit 19", btsdp.MASRecord{Channel: 5, HasFeatures: true, Features: 0x0000027f}, head},
		{"bit 19 without attribute flag", btsdp.MASRecord{Channel: 5, Features: 1 << 19}, head},
		{"present with bit 19", btsdp.MASRecord{Channel: 5, HasFeatures: true, Features: 1<<19 | 0x7f}, withFeat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			f := newFakeEnd(t, b)
			got := make(chan []byte, 1)
			go func() { got <- f.recvRaw(); f.connectOK(1) }()
			m, err := ConnectMAS(context.Background(), a, tc.rec)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			want := append([]byte(nil), tc.want...)
			want[2] = byte(len(want))
			if g := <-got; !bytes.Equal(g, want) {
				t.Fatalf("got % X\nwant % X", g, want)
			}
		})
	}
}

func TestConnectMASRefusedClosesTransport(t *testing.T) {
	a, b := net.Pipe()
	f := newFakeEnd(t, b)
	go func() { f.recv(); f.send(obex.Packet{Code: obex.RespForbidden}) }()
	_, err := ConnectMAS(context.Background(), a, btsdp.MASRecord{})
	var re *obex.ResponseError
	if !errors.As(err, &re) || re.Code != obex.RespForbidden {
		t.Fatalf("got %v", err)
	}
	select {
	case <-f.c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport not closed after a refused connect")
	}
}

func TestConnectMASCancelDuringStalledConnect(t *testing.T) {
	a, b := net.Pipe()
	f := newFakeEnd(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := ConnectMAS(ctx, a, btsdp.MASRecord{}); res <- err }()
	f.recv() // the request arrived; no answer follows
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConnectMAS did not return after cancel")
	}
	select {
	case <-f.c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport not closed")
	}
}

func TestRegisterNotificationsBytes(t *testing.T) {
	m, f := mustConnectMAS(t)
	got := make(chan []byte, 1)
	go func() { got <- f.recvRaw(); f.send(obex.Packet{Code: obex.RespSuccess}) }()
	if err := m.RegisterNotifications(); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x82, 0, 0, 0xCB, 0, 0, 0, 7}
	want = append(want, 0x42, 0, byte(3+len(typeNotificationRegistration)+1))
	want = append(want, typeNotificationRegistration...)
	want = append(want, 0, 0x4C, 0, 6, 0x0E, 1, 1, 0x49, 0, 4, 0x30)
	want[2] = byte(len(want))
	if g := <-got; !bytes.Equal(g, want) {
		t.Fatalf("got % X\nwant % X", g, want)
	}
}

func TestRegisterNotificationsRefused(t *testing.T) {
	m, f := mustConnectMAS(t)
	go func() { f.recv(); f.send(obex.Packet{Code: obex.RespServiceUnavailable}) }()
	err := m.RegisterNotifications()
	var re *obex.ResponseError
	if !errors.As(err, &re) || re.Code != obex.RespServiceUnavailable {
		t.Fatalf("got %v", err)
	}
}

const sampleBMsg = "BEGIN:BMSG\r\nVERSION:1.0\r\nSTATUS:UNREAD\r\nTYPE:SMS_GSM\r\nFOLDER:TELECOM/MSG/INBOX\r\n" +
	"BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Jane\r\nTEL:+1555\r\nEND:VCARD\r\n" +
	"BEGIN:BENV\r\nBEGIN:BBODY\r\nCHARSET:UTF-8\r\nLENGTH:20\r\nBEGIN:MSG\r\nHello\r\nEND:MSG\r\nEND:BBODY\r\nEND:BENV\r\nEND:BMSG\r\n"

func TestGetMessage(t *testing.T) {
	m, f := mustConnectMAS(t)
	got := make(chan []byte, 1)
	go func() {
		got <- f.recvRaw()
		f.send(obex.Packet{Code: obex.RespSuccess, Headers: obex.Headers{obex.Bytes(obex.HEndOfBody, []byte(sampleBMsg))}})
	}()
	bm, err := m.GetMessage("0400000000000012")
	if err != nil {
		t.Fatal(err)
	}
	if bm.From != "+1555" || bm.FromName != "Jane" || bm.Text != "Hello" || bm.Type != "SMS_GSM" {
		t.Fatalf("got %+v", bm)
	}
	want := []byte{0x83, 0, 0, 0xCB, 0, 0, 0, 7}
	name := []byte{0x01, 0, byte(3 + 2*16 + 2)}
	for _, r := range "0400000000000012" {
		name = append(name, 0, byte(r))
	}
	name = append(name, 0, 0)
	want = append(want, name...)
	want = append(want, 0x42, 0, byte(3+len(typeMessage)+1))
	want = append(want, typeMessage...)
	want = append(want, 0, 0x4C, 0, 9, 0x0A, 1, 0, 0x14, 1, 1)
	want[2] = byte(len(want))
	if g := <-got; !bytes.Equal(g, want) {
		t.Fatalf("got % X\nwant % X", g, want)
	}
}

func TestGetMessageParseErrorAndRefusal(t *testing.T) {
	m, f := mustConnectMAS(t)
	go func() {
		f.recv()
		f.send(obex.Packet{Code: obex.RespSuccess, Headers: obex.Headers{obex.Bytes(obex.HEndOfBody, []byte("SECRETWORD"))}})
		f.recv()
		f.send(obex.Packet{Code: 0xCC})
	}()
	_, err := m.GetMessage("1")
	if ClassifyGetError(err) != GetDrop || strings.Contains(err.Error(), "SECRETWORD") {
		t.Fatalf("parse error: %v (%v)", err, ClassifyGetError(err))
	}
	_, err = m.GetMessage("2")
	if ClassifyGetError(err) != GetDrop {
		t.Fatalf("0xCC: %v", err)
	}
}

func TestClassifyGetError(t *testing.T) {
	re := func(c byte) error { return fmt.Errorf("wrap: %w", &obex.ResponseError{Code: c}) }
	tests := []struct {
		name string
		err  error
		want GetAction
	}{
		{"nil", nil, GetDeliver},
		{"unauthorized", re(obex.RespUnauthorized), GetEndSession},
		{"internal error", re(obex.RespInternalError), GetRetry},
		{"service unavailable", re(obex.RespServiceUnavailable), GetRetry},
		{"not found", re(obex.RespNotFound), GetDrop},
		{"bad request", re(obex.RespBadRequest), GetDrop},
		{"not acceptable", re(obex.RespNotAcceptable), GetDrop},
		{"forbidden", re(obex.RespForbidden), GetDrop},
		{"precondition failed", re(0xCC), GetDrop},
		{"not implemented", re(obex.RespNotImplemented), GetDrop},
		{"unlisted code", re(0xE0), GetDrop},
		{"protocol", fmt.Errorf("x: %w", obex.ErrProtocol), GetEndSession},
		{"timeout", fmt.Errorf("x: %w", obex.ErrTimeout), GetEndSession},
		{"too large", fmt.Errorf("x: %w", obex.ErrTooLarge), GetEndSession},
		{"eof", io.EOF, GetEndSession},
		{"unexpected eof", io.ErrUnexpectedEOF, GetEndSession},
		{"deadline", context.DeadlineExceeded, GetEndSession},
		{"canceled", context.Canceled, GetEndSession},
		{"unsupported", fmt.Errorf("x: %w", ErrUnsupported), GetDrop},
		{"malformed", fmt.Errorf("x: %w", ErrMalformed), GetDrop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyGetError(tc.err); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
