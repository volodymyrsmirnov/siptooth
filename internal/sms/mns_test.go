package sms

import (
	"bytes"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

type mnsCall struct {
	instance int
	evs      []Event
	err      error
}

// fakePhone connects to a ServeMNS running on the other end of a net.Pipe.
func startMNS(t *testing.T) (*fakeEnd, chan mnsCall, chan struct{}, chan error) {
	t.Helper()
	a, b := net.Pipe()
	f := newFakeEnd(t, b)
	calls := make(chan mnsCall, 8)
	connected := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- ServeMNS(a, func() { connected <- struct{}{} }, func(i int, e []Event, err error) { calls <- mnsCall{i, e, err} })
	}()
	return f, calls, connected, done
}

func (f *fakeEnd) mnsConnect() {
	f.send(obex.Packet{Code: obex.OpConnect, Connect: &obex.ConnectFields{Version: 0x10, MaxPacket: 1000},
		Headers: obex.Headers{obex.Bytes(obex.HTarget, MNSTarget)}})
	p, err := obex.ParseResponse(f.recvRaw(), true)
	if err != nil || p.Code != obex.RespSuccess {
		f.t.Fatalf("connect: %v %+v", err, p)
	}
}

func (f *fakeEnd) putEvent(typ string, ap []byte, body string) byte {
	hs := obex.Headers{obex.U32(obex.HConnectionID, 1), obex.ASCII(obex.HType, typ)}
	if ap != nil {
		hs = append(hs, obex.Bytes(obex.HAppParams, ap))
	}
	hs = append(hs, obex.Bytes(obex.HEndOfBody, []byte(body)))
	f.send(obex.Packet{Code: obex.OpPutFinal, Headers: hs})
	p, err := obex.ParseResponse(f.recvRaw(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	return p.Code
}

const oneEvent = `<MAP-event-report version="1.0"><event type="NewMessage" handle="0123" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/></MAP-event-report>`

func TestServeMNS(t *testing.T) {
	f, calls, connected, done := startMNS(t)
	f.mnsConnect()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("onConnected not called")
	}
	want := []Event{{"NewMessage", "0123", "TELECOM/MSG/INBOX", "SMS_GSM"}}

	// With and without a MASInstanceID.
	if c := f.putEvent(typeEventReport, obex.EncodeAppParams(obex.AppParam{Tag: 0x0F, Value: []byte{3}}), oneEvent); c != obex.RespSuccess {
		t.Fatalf("code %02X", c)
	}
	if c := <-calls; c.instance != 3 || c.err != nil || !reflect.DeepEqual(c.evs, want) {
		t.Fatalf("got %+v", c)
	}
	if c := f.putEvent(typeEventReport, nil, oneEvent); c != obex.RespSuccess {
		t.Fatalf("code %02X", c)
	}
	if c := <-calls; c.instance != -1 || c.err != nil || !reflect.DeepEqual(c.evs, want) {
		t.Fatalf("got %+v", c)
	}

	// A malformed report still gets Success; the error reaches the callback without the body.
	if c := f.putEvent(typeEventReport, nil, "SECRETWORD<"); c != obex.RespSuccess {
		t.Fatalf("code %02X", c)
	}
	if c := <-calls; c.err == nil || bytes.Contains([]byte(c.err.Error()), []byte("SECRETWORD")) {
		t.Fatalf("got %+v", c)
	}

	// A mismatched instance is the service's call: the ID is passed through.
	f.putEvent(typeEventReport, obex.EncodeAppParams(obex.AppParam{Tag: 0x0F, Value: []byte{9}}), oneEvent)
	if c := <-calls; c.instance != 9 {
		t.Fatalf("got %+v", c)
	}

	// Another Type is Bad Request and never reaches the callback.
	if c := f.putEvent("x-bt/message", nil, oneEvent); c != obex.RespBadRequest {
		t.Fatalf("code %02X", c)
	}
	select {
	case c := <-calls:
		t.Fatalf("unexpected callback %+v", c)
	default:
	}

	f.send(obex.Packet{Code: obex.OpDisconnect, Headers: obex.Headers{obex.U32(obex.HConnectionID, 1)}})
	f.recvRaw()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeMNS did not return")
	}
}

func TestServeMNSNoEventConnectedCallback(t *testing.T) {
	f, calls, connected, done := startMNS(t)
	f.mnsConnect()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("onConnected not called without any event")
	}
	f.c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the EOF reason")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeMNS did not return on EOF")
	}
	if len(calls) != 0 {
		t.Fatal("unexpected event callback")
	}
}

func TestServeMNSWrongTarget(t *testing.T) {
	f, _, connected, _ := startMNS(t)
	f.send(obex.Packet{Code: obex.OpConnect, Connect: &obex.ConnectFields{Version: 0x10, MaxPacket: 1000},
		Headers: obex.Headers{obex.Bytes(obex.HTarget, MASTarget)}})
	p, _ := obex.ParseResponse(f.recvRaw(), true)
	if p.Code != obex.RespForbidden {
		t.Fatalf("code %02X", p.Code)
	}
	select {
	case <-connected:
		t.Fatal("onConnected called for a refused connect")
	default:
	}
}
