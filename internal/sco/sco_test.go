package sco

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestParseAndPackAddr(t *testing.T) {
	a, err := ParseAddr("AA:BB:CC:DD:EE:FF")
	if err != nil {
		t.Fatal(err)
	}
	if FormatAddr(a) != "AA:BB:CC:DD:EE:FF" {
		t.Fatal(FormatAddr(a))
	}
	sa := packSockaddr(a)
	// family 31 little-endian, then bdaddr reversed (LSB first)
	want := [8]byte{31, 0, 0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA}
	if sa != want {
		t.Fatalf("%x", sa)
	}
	if _, err := ParseAddr("zz"); err == nil {
		t.Fatal("expected error")
	}
	if a2, _ := ParseAddr("aa:bb:cc:dd:ee:ff"); a2 != a {
		t.Fatal("lowercase should parse identically")
	}
}

func TestPipeFramesAndMTU(t *testing.T) {
	a, b := NewPipe(64)
	defer a.Close()
	defer b.Close()
	if a.MTU() != 64 || b.Remote() == "" {
		t.Fatal("mtu/remote")
	}
	frame := bytes.Repeat([]byte{1, 2}, 32)
	go a.Write(frame)
	buf := make([]byte, 128)
	n, err := b.Read(buf)
	if err != nil || n != 64 || !bytes.Equal(buf[:n], frame) {
		t.Fatal(n, err)
	}
	if _, err := a.Write(make([]byte, 65)); err == nil {
		t.Fatal("oversized write must fail like the kernel (EINVAL)")
	}
	a.Close()
	if _, err := b.Read(buf); err == nil {
		t.Fatal("read after peer close must fail")
	}
}

func TestListenerUnsupportedOffLinux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Listen("AA:BB:CC:DD:EE:FF")
	if err == nil {
		t.Skip("running on linux with a real adapter")
	}
	_ = ctx
}

func TestPipeWriteAfterPeerCloseFails(t *testing.T) {
	for i := 0; i < 200; i++ {
		a, b := NewPipe(8)
		b.Close()
		if _, err := a.Write([]byte{1}); err == nil {
			t.Fatalf("iteration %d: write after peer close must fail", i)
		}
		a.Close()
	}
}

// An in-band ringing link is accepted by the kernel while the phone rings; Drain discards that
// backlog so the first paced write starts from live audio.
func TestDrainDiscardsBacklog(t *testing.T) {
	a, b := NewPipe(64)
	defer a.Close()
	defer b.Close()
	for i := 0; i < 5; i++ {
		if _, err := b.Write(bytes.Repeat([]byte{9}, 64)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Drain(a)
	if err != nil || n != 5 {
		t.Fatalf("drained %d, %v", n, err)
	}
	if n, _ := Drain(a); n != 0 {
		t.Fatalf("second drain %d", n)
	}
	go b.Write(bytes.Repeat([]byte{7}, 64))
	buf := make([]byte, 64)
	if n, err := a.Read(buf); err != nil || n != 64 || buf[0] != 7 {
		t.Fatalf("live frame after drain: %d %v %v", n, err, buf[0])
	}
}
