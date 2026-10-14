package siptest

import (
	"net"
	"net/netip"
	"testing"
)

// RTPEcho starts a UDP echo server on loopback: every datagram is sent back
// to its source unchanged. It stands in for the far end's media socket.
func RTPEcho(t testing.TB) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("siptest: RTP echo listen: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}
