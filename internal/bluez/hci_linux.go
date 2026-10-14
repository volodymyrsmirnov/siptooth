//go:build linux

package bluez

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/unix"
)

const (
	solHCI    = 0
	hciFilter = 2
)

type hciSocket struct{ fd int }

func openHCI(index int) (*hciSocket, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("hci socket (needs CAP_NET_RAW): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: uint16(index), Channel: unix.HCI_CHANNEL_RAW}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hci bind hci%d: %w", index, err)
	}
	if err := unix.SetsockoptString(fd, solHCI, hciFilter, string(hciFilterBytes())); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hci filter: %w", err)
	}
	return &hciSocket{fd: fd}, nil
}

func (h *hciSocket) close() { unix.Close(h.fd) }

// command sends one HCI command and waits up to 1 s for its Command Complete.
// A Command Status for the same opcode with a non-zero status fails immediately.
func (h *hciSocket) command(pkt []byte, opcode uint16) (byte, []byte, error) {
	if _, err := unix.Write(h.fd, pkt); err != nil {
		return 0, nil, fmt.Errorf("hci write: %w", err)
	}
	deadline := time.Now().Add(time.Second)
	buf := make([]byte, 260)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, nil, fmt.Errorf("hci: timeout waiting for command complete %#x", opcode)
		}
		fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining/time.Millisecond)+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("hci poll: %w", err)
		}
		if n == 0 || fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return 0, nil, fmt.Errorf("hci poll: revents %#x", fds[0].Revents)
			}
			continue
		}
		m, err := unix.Read(h.fd, buf)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("hci read: %w", err)
		}
		if st, ok := parseCommandStatus(buf[:m], opcode); ok {
			if st != 0 {
				return 0, nil, fmt.Errorf("hci: command %#x rejected with status %#x", opcode, st)
			}
			continue
		}
		status, params, perr := parseCommandComplete(buf[:m], opcode)
		if perr != nil {
			continue // some other event; keep waiting
		}
		return status, params, nil
	}
}

// readPCMParams issues Read_SCO_PCM_Int_Param and returns the 5 parameter bytes.
func (h *hciSocket) readPCMParams() ([5]byte, error) {
	var out [5]byte
	status, params, err := h.command(buildReadPCMParams(), opcodeReadPCMParams)
	if err != nil {
		return out, err
	}
	if status != 0 || len(params) < 5 {
		return out, fmt.Errorf("hci: read pcm params status %#x len %d", status, len(params))
	}
	copy(out[:], params[:5])
	return out, nil
}

// ReadSCORouting returns the current sco_routing value (0 PCM, 1 HCI, 2 codec, 3 I2S).
func ReadSCORouting(hciIndex int) (uint8, error) {
	h, err := openHCI(hciIndex)
	if err != nil {
		return 0, err
	}
	defer h.close()
	cur, err := h.readPCMParams()
	if err != nil {
		return 0, err
	}
	return cur[0], nil
}

// EnsureSCORouting routes SCO over HCI when it is not already (spec section 3.5). A failure
// to read the current value is a *RoutingReadError; JudgeRouting interprets the result.
func EnsureSCORouting(hciIndex int, logger *slog.Logger) (uint8, uint8, error) {
	if logger == nil {
		logger = slog.Default()
	}
	h, err := openHCI(hciIndex)
	if err != nil {
		return 0, 0, &RoutingReadError{Err: err}
	}
	defer h.close()
	cur, err := h.readPCMParams()
	if err != nil {
		return 0, 0, &RoutingReadError{Err: err}
	}
	before := cur[0]
	if before == scoRoutingHCI {
		logger.Info("bluez: SCO already routed over HCI", "params", fmt.Sprintf("%x", cur))
		return before, before, nil
	}
	want := cur
	want[0] = scoRoutingHCI
	status, _, err := h.command(buildWritePCMParams(want), opcodeWritePCMParams)
	if err != nil {
		return before, before, err
	}
	if status != 0 {
		return before, before, fmt.Errorf("hci: write pcm params status %#x", status)
	}
	got, err := h.readPCMParams()
	if err != nil {
		return before, before, fmt.Errorf("hci: verify: %w", err)
	}
	after := got[0]
	if after != scoRoutingHCI {
		logger.Warn("bluez: SCO routing write did not take effect", "before", before, "after", after, "params", fmt.Sprintf("%x", got))
		return before, after, fmt.Errorf("hci: verify: sco_routing is %d after write, want %d", after, scoRoutingHCI)
	}
	logger.Info("bluez: SCO routing changed", "before", before, "after", after, "params", fmt.Sprintf("%x", got))
	return before, after, nil
}
