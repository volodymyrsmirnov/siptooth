package bluez

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	opcodeWritePCMParams uint16 = 0xFC1C // Broadcom Write_SCO_PCM_Int_Param
	opcodeReadPCMParams  uint16 = 0xFC1D // Broadcom Read_SCO_PCM_Int_Param
	hciCommandPkt               = 0x01
	hciEventPkt                 = 0x04
	evtCommandComplete          = 0x0E
	evtCommandStatus            = 0x0F
	scoRoutingHCI        uint8  = 1
)

func buildReadPCMParams() []byte {
	b := []byte{hciCommandPkt, 0, 0, 0}
	binary.LittleEndian.PutUint16(b[1:], opcodeReadPCMParams)
	return b
}

func buildWritePCMParams(p [5]byte) []byte {
	b := []byte{hciCommandPkt, 0, 0, 5}
	binary.LittleEndian.PutUint16(b[1:], opcodeWritePCMParams)
	return append(b, p[:]...)
}

// parseCommandComplete validates an HCI event packet and returns status and return parameters.
func parseCommandComplete(pkt []byte, opcode uint16) (byte, []byte, error) {
	if len(pkt) < 7 {
		return 0, nil, errors.New("hci: short packet")
	}
	if pkt[0] != hciEventPkt || pkt[1] != evtCommandComplete {
		return 0, nil, fmt.Errorf("hci: not a command complete event (type %#x event %#x)", pkt[0], pkt[1])
	}
	end := 3 + int(pkt[2])
	if end < 7 || end > len(pkt) {
		return 0, nil, fmt.Errorf("hci: bad event length %d for %d-byte packet", pkt[2], len(pkt))
	}
	if got := binary.LittleEndian.Uint16(pkt[4:6]); got != opcode {
		return 0, nil, fmt.Errorf("hci: opcode %#x, want %#x", got, opcode)
	}
	return pkt[6], pkt[7:end], nil
}

// parseCommandStatus reports whether pkt is a Command Status event for opcode and returns its status.
func parseCommandStatus(pkt []byte, opcode uint16) (byte, bool) {
	// 04 0F 04 <status> <num_hci_cmd> <opcode LE>
	if len(pkt) < 7 || pkt[0] != hciEventPkt || pkt[1] != evtCommandStatus || pkt[2] < 4 {
		return 0, false
	}
	if binary.LittleEndian.Uint16(pkt[5:7]) != opcode {
		return 0, false
	}
	return pkt[3], true
}

// hciFilterBytes builds the kernel's struct hci_ufilter accepting Command Complete and
// Command Status events: type_mask [0:4], event_mask [4:12], opcode [12:14], padding [14:16].
// It must be the full 16 bytes (sizeof with u32 alignment); kernels with the
// CVE-2024-35963 fix reject a shorter optlen with EINVAL.
func hciFilterBytes() []byte {
	f := make([]byte, 16)
	binary.LittleEndian.PutUint32(f[0:], 1<<hciEventPkt)
	binary.LittleEndian.PutUint32(f[4:], (1<<evtCommandComplete)|(1<<evtCommandStatus))
	return f
}
