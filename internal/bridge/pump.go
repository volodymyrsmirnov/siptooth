// Package bridge connects the phone's SCO audio to the SIP leg's RTP session.
package bridge

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
)

const (
	rtpFrameSamples = 160 // 20 ms at 8 kHz
	rtpFrameDur     = 20 * time.Millisecond
	stallAfter      = 100 * time.Millisecond
	stallLimit      = 5 * time.Second
)

// RTP is the part of an RTP session the pump uses.
type RTP interface {
	WriteFrame(pcm []int16) error
	WriteSilence() error
	Playout() *rtp.Playout
	Stats() rtp.Stats
}

// PumpEnd says why RunPump returned.
type PumpEnd string

const (
	PumpCtx      PumpEnd = "ctx"
	PumpSCOEOF   PumpEnd = "sco_eof"
	PumpSCOStall PumpEnd = "sco_stall"
	PumpRTPError PumpEnd = "rtp_error"
)

// RunPump bridges SCO and RTP until ctx ends or a link fails (spec 6.2). Pacing
// is read-driven: every SCO read is answered with the same number of bytes from
// the playout. Without reads for 100 ms a ticker keeps the downlink and RTP
// uplink going; after 5 s without reads it returns PumpSCOStall. A SCO write
// error counts as PumpSCOEOF. For the first settle after start the pump writes
// silence to SCO and leaves the playout in startup mode, then ends startup mode
// once. The caller closes link to unblock the reader after RunPump returns.
func RunPump(ctx context.Context, link sco.Link, r RTP, settle time.Duration, clk Clock) PumpEnd {
	mtu := link.MTU()
	frames := make(chan []byte, 8)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			buf := make([]byte, mtu)
			n, err := link.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- buf[:n]:
			case <-done:
				return
			}
		}
	}()

	pl := r.Playout()
	settled := false
	var settleC <-chan time.Time
	if settle <= 0 {
		pl.SetStartup(false)
		settled = true
	} else {
		t := clk.NewTimer(settle)
		defer t.Stop()
		settleC = t.C()
	}

	frameDur := time.Duration(mtu) * time.Second / 16000 // 16 PCM bytes per ms; 60 B = 3.75 ms
	if frameDur <= 0 {
		frameDur = 4 * time.Millisecond
	}
	stallT := clk.NewTicker(frameDur)
	defer stallT.Stop()
	silenceT := clk.NewTicker(rtpFrameDur)
	defer silenceT.Stop()

	lastRead := clk.Now()
	pending := make([]int16, 0, 2*rtpFrameSamples)

	// down writes n bytes of downlink audio to SCO.
	down := func(n int) error {
		out := make([]byte, n&^1)
		if settled {
			pcm := make([]int16, len(out)/2)
			pl.Read(pcm)
			for i, s := range pcm {
				binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
			}
		}
		_, err := link.Write(out)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return PumpCtx
		case <-readErr:
			return PumpSCOEOF
		case <-settleC:
			settleC = nil
			pl.SetStartup(false)
			settled = true
		case frame := <-frames:
			lastRead = clk.Now()
			for i := 0; i+1 < len(frame); i += 2 {
				pending = append(pending, int16(binary.LittleEndian.Uint16(frame[i:])))
			}
			for len(pending) >= rtpFrameSamples {
				if err := r.WriteFrame(pending[:rtpFrameSamples]); err != nil {
					return PumpRTPError
				}
				pending = append(pending[:0], pending[rtpFrameSamples:]...)
			}
			if err := down(len(frame)); err != nil {
				return PumpSCOEOF
			}
		case <-stallT.C():
			idle := clk.Now().Sub(lastRead)
			if idle >= stallLimit {
				return PumpSCOStall
			}
			if idle > stallAfter {
				if err := down(mtu); err != nil {
					return PumpSCOEOF
				}
			}
		case <-silenceT.C():
			if clk.Now().Sub(lastRead) > stallAfter {
				if err := r.WriteSilence(); err != nil {
					return PumpRTPError
				}
			}
		}
	}
}
