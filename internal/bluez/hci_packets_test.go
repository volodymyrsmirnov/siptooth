package bluez

import (
	"bytes"
	"testing"
)

func TestBuildCommands(t *testing.T) {
	if got := buildReadPCMParams(); !bytes.Equal(got, []byte{0x01, 0x1D, 0xFC, 0x00}) {
		t.Fatalf("%x", got)
	}
	got := buildWritePCMParams([5]byte{1, 2, 0, 1, 1})
	if !bytes.Equal(got, []byte{0x01, 0x1C, 0xFC, 0x05, 1, 2, 0, 1, 1}) {
		t.Fatalf("%x", got)
	}
}

func TestParseCommandComplete(t *testing.T) {
	// 04 = event packet, 0E = Command Complete, len, num_hci_cmd, opcode LE, status, params
	pkt := []byte{0x04, 0x0E, 0x09, 0x01, 0x1D, 0xFC, 0x00, 0x00, 0x02, 0x00, 0x01, 0x01}
	status, params, err := parseCommandComplete(pkt, 0xFC1D)
	if err != nil || status != 0 || !bytes.Equal(params, []byte{0, 2, 0, 1, 1}) {
		t.Fatal(status, params, err)
	}
	if _, _, err := parseCommandComplete(pkt, 0xFC1C); err == nil {
		t.Fatal("opcode mismatch must error")
	}
	if _, _, err := parseCommandComplete([]byte{0x04, 0x0F, 0x04, 0x00, 0x01, 0x1D, 0xFC}, 0xFC1D); err == nil {
		t.Fatal("command status is not command complete")
	}
	if _, _, err := parseCommandComplete([]byte{0x04}, 0xFC1D); err == nil {
		t.Fatal("short packet")
	}
}

func TestHCIFilterBytes(t *testing.T) {
	f := hciFilterBytes()
	// sizeof(struct hci_ufilter) is 16 (u32 alignment padding after the u16 opcode)
	if len(f) != 16 {
		t.Fatal(len(f))
	}
	// type_mask bit 4 (event packets); event_mask[0] bits 0x0E and 0x0F
	if f[0] != 1<<4 || f[4] != 0x00 || f[5] != 0xC0 {
		t.Fatalf("%x", f)
	}
	for i, b := range f[6:] {
		if b != 0 {
			t.Fatalf("byte %d = %#x, want 0: %x", i+6, b, f)
		}
	}
}

func TestParseCommandStatus(t *testing.T) {
	pkt := []byte{0x04, 0x0F, 0x04, 0x01, 0x01, 0x1C, 0xFC}
	if st, ok := parseCommandStatus(pkt, 0xFC1C); !ok || st != 0x01 {
		t.Fatal(st, ok)
	}
	if _, ok := parseCommandStatus(pkt, 0xFC1D); ok {
		t.Fatal("opcode mismatch must not match")
	}
	if _, ok := parseCommandStatus([]byte{0x04, 0x0E, 0x04, 0x01, 0x1C, 0xFC, 0x00}, 0xFC1C); ok {
		t.Fatal("command complete is not command status")
	}
}

func TestParseCommandCompleteBadLength(t *testing.T) {
	// declared length 0x0A exceeds the 9 bytes actually present
	pkt := []byte{0x04, 0x0E, 0x0A, 0x01, 0x1D, 0xFC, 0x00, 0x00, 0x02, 0x00, 0x01, 0x01}
	if _, _, err := parseCommandComplete(pkt, 0xFC1D); err == nil {
		t.Fatal("truncated event must error")
	}
}

func TestParseCommandCompleteDeclaredLengthTooShort(t *testing.T) {
	// declared length 3 cannot hold num_hci_cmd + opcode + status
	pkt := []byte{0x04, 0x0E, 0x03, 0x01, 0x1D, 0xFC, 0x00, 0x00, 0x02, 0x00, 0x01, 0x01}
	if _, _, err := parseCommandComplete(pkt, 0xFC1D); err == nil {
		t.Fatal("declared length < 4 must error")
	}
}
