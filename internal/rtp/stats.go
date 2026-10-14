package rtp

import (
	"net/netip"
	"time"
)

// Stats is a snapshot of the session counters. Sample-valued counters
// (Overflow, DiscardedSilence) count 8 kHz samples.
type Stats struct {
	Sent             uint64 // RTP packets sent
	Received         uint64 // valid packets accepted (after latch/PT/SRTP checks)
	Lost             uint64 // sequence-number gaps not filled by reordered packets
	Late             uint64 // packets entirely before the playout point
	Duplicate        uint64 // packets whose samples were already buffered
	Overflow         uint64 // samples dropped (oldest first) at the buffer cap
	DiscardedSilence uint64 // samples of silence skipped by catch-up
	SRTPFail         uint64 // SRTP authentication or replay failures
	OtherPT          uint64 // packets with a payload type we do not decode
	LastRx           time.Time
	Latched          netip.AddrPort // zero until the first valid packet
	MaxGap           time.Duration  // largest gap between consecutive accepted packets after Start
}
