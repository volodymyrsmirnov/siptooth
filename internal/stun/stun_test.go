package stun

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

// RFC 5769 section 2.2: sample IPv4 response.
var rfcTxID = [12]byte{0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae}

var rfcResponse = []byte{
	0x01, 0x01, 0x00, 0x3c, 0x21, 0x12, 0xa4, 0x42,
	0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae,
	0x80, 0x22, 0x00, 0x0b, 0x74, 0x65, 0x73, 0x74, 0x20, 0x76, 0x65, 0x63, 0x74, 0x6f, 0x72, 0x20,
	0x00, 0x20, 0x00, 0x08, 0x00, 0x01, 0xa1, 0x47, 0xe1, 0x12, 0xa6, 0x43,
	0x00, 0x08, 0x00, 0x14, 0x2b, 0x91, 0xf5, 0x99, 0xfd, 0x9e, 0x90, 0xc3, 0x8c, 0x74, 0x89, 0xf9,
	0x2a, 0xf9, 0xba, 0x53, 0xf0, 0x6b, 0xe7, 0xd7,
	0x80, 0x28, 0x00, 0x04, 0xc8, 0xfb, 0x0b, 0x4c,
}

func TestParseRFC5769(t *testing.T) {
	got, err := ParseBindingResponse(rfcResponse, rfcTxID)
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddrPort("192.0.2.1:32853"); got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseTruncatedNeverPanics(t *testing.T) {
	for n := 0; n < len(rfcResponse); n++ {
		if _, err := ParseBindingResponse(rfcResponse[:n], rfcTxID); err == nil {
			t.Fatalf("prefix of %d bytes parsed without error", n)
		}
	}
}

func TestParseAttributeLengthBeyondBuffer(t *testing.T) {
	b := append([]byte(nil), rfcResponse[:20]...)
	b = append(b, 0x00, 0x20, 0x00, 0xff, 0x00, 0x01) // attr claims 255 bytes
	binary.BigEndian.PutUint16(b[2:4], 6)
	if _, err := ParseBindingResponse(b, rfcTxID); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseWrongTxID(t *testing.T) {
	other := rfcTxID
	other[0] ^= 0xff
	if _, err := ParseBindingResponse(rfcResponse, other); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseMappedAddressFallback(t *testing.T) {
	b := []byte{0x01, 0x01, 0x00, 0x0c, 0x21, 0x12, 0xa4, 0x42}
	b = append(b, rfcTxID[:]...)
	b = append(b, 0x00, 0x01, 0x00, 0x08, 0x00, 0x01, 0x1f, 0x90, 198, 51, 100, 7)
	got, err := ParseBindingResponse(b, rfcTxID)
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddrPort("198.51.100.7:8080"); got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseNoAddressAttribute(t *testing.T) {
	b := append([]byte(nil), rfcResponse[:20]...)
	binary.BigEndian.PutUint16(b[2:4], 0)
	if _, err := ParseBindingResponse(b, rfcTxID); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseErrorResponseRejected(t *testing.T) {
	b := append([]byte(nil), rfcResponse...)
	b[0], b[1] = 0x01, 0x11
	if _, err := ParseBindingResponse(b, rfcTxID); err == nil {
		t.Fatal("expected error for non-success type")
	}
}

func TestIsSTUN(t *testing.T) {
	if !IsSTUN(rfcResponse) {
		t.Fatal("vector should be STUN")
	}
	rtp := []byte{0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xa0, 0x12, 0x34, 0x56, 0x78, 0xff, 0xff}
	if IsSTUN(rtp) {
		t.Fatal("RTP is not STUN")
	}
	if IsSTUN(rfcResponse[:19]) || IsSTUN(nil) {
		t.Fatal("short input is not STUN")
	}
}

func TestNewBindingRequest(t *testing.T) {
	txID, msg := NewBindingRequest()
	if len(msg) != 20 || !IsSTUN(msg) {
		t.Fatalf("bad request % x", msg)
	}
	if binary.BigEndian.Uint16(msg[0:2]) != 0x0001 || binary.BigEndian.Uint16(msg[2:4]) != 0 {
		t.Fatalf("bad header % x", msg)
	}
	if !bytes.Equal(msg[8:20], txID[:]) {
		t.Fatal("txID mismatch")
	}
	txID2, _ := NewBindingRequest()
	if txID == txID2 {
		t.Fatal("txIDs must be random")
	}
}

func response(txID [12]byte, ap netip.AddrPort) []byte {
	b := []byte{0x01, 0x01, 0x00, 0x0c, 0x21, 0x12, 0xa4, 0x42}
	b = append(b, txID[:]...)
	b = append(b, 0x00, 0x20, 0x00, 0x08, 0x00, 0x01)
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], ap.Port()^0x2112)
	b = append(b, port[:]...)
	ip := ap.Addr().As4()
	cookie := []byte{0x21, 0x12, 0xa4, 0x42}
	for i := range ip {
		b = append(b, ip[i]^cookie[i])
	}
	return b
}

func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestQuery(t *testing.T) {
	srv := listen(t)
	want := netip.MustParseAddrPort("203.0.113.9:40000")
	go func() {
		buf := make([]byte, 1500)
		n, from, err := srv.ReadFromUDP(buf)
		if err != nil || n < 20 {
			return
		}
		var tx [12]byte
		copy(tx[:], buf[8:20])
		// Noise first: a non-matching packet must be ignored.
		srv.WriteToUDP([]byte{0x80, 0, 0, 1}, from)
		srv.WriteToUDP(response(tx, want), from)
	}()
	cli := listen(t)
	got, err := Query(cli, srv.LocalAddr().String(), time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestQueryRetriesAfterLostFirstRequest(t *testing.T) {
	srv := listen(t)
	want := netip.MustParseAddrPort("203.0.113.9:40001")
	go func() {
		buf := make([]byte, 1500)
		srv.ReadFromUDP(buf) // drop the first request
		n, from, err := srv.ReadFromUDP(buf)
		if err != nil || n < 20 {
			return
		}
		var tx [12]byte
		copy(tx[:], buf[8:20])
		srv.WriteToUDP(response(tx, want), from)
	}()
	cli := listen(t)
	got, err := Query(cli, srv.LocalAddr().String(), 150*time.Millisecond, 2)
	if err != nil || got != want {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestQueryTimeout(t *testing.T) {
	srv := listen(t)
	cli := listen(t)
	start := time.Now()
	_, err := Query(cli, srv.LocalAddr().String(), 100*time.Millisecond, 2)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	el := time.Since(start)
	if el < 190*time.Millisecond || el > 2*time.Second {
		t.Fatalf("elapsed %v, want about attempts*timeout", el)
	}
}
