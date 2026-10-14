// Package bridge connects the phone's SCO audio to the SIP leg's RTP session.
package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/echo"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
)

const (
	rtpFrameSamples = 160 // 20 ms at 8 kHz
	rtpFrameDur     = 20 * time.Millisecond
	stallAfter      = 100 * time.Millisecond
	stallLimit      = 5 * time.Second
	rtpFailLimit    = 5 * time.Second // transient RTP send failures tolerated before giving up
	rtpWarnEvery    = time.Second
	watchdogEvery   = 250 * time.Millisecond
	unblockWait     = 500 * time.Millisecond // real time to let a closed link release the loop
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

// pumpPrefix starts the PumpEnd of a pump that panicked; the rest is the
// panic text.
const pumpPrefix = "panic: "

func pumpPanic(v any) PumpEnd { return PumpEnd(pumpPrefix + fmt.Sprint(v)) }

// RunPump bridges SCO and RTP until ctx ends or a link fails (spec 6.2). Pacing
// is read-driven: every SCO read is answered with the same number of bytes from
// the playout. Without reads for 100 ms a ticker keeps the downlink and RTP
// uplink going; after 5 s without reads it returns PumpSCOStall. A SCO write
// error counts as PumpSCOEOF. A permanent RTP error (closed socket, session not
// running) returns PumpRTPError at once; other send errors are tolerated, logged
// at most once a second, and never skip the downlink write, and only failures
// lasting 5 s without a single success return PumpRTPError. For the first settle after start the pump writes
// silence to SCO and leaves the playout in startup mode, then ends startup mode
// once. A non-nil gate (spec 6.6) sees every block written to SCO as its
// reference and gates every uplink frame before it is sent. A blocked link.Write delays the reaction to ctx; the caller unblocks it
// (and the reader goroutine, after RunPump returns) by closing the link.
func RunPump(ctx context.Context, link sco.Link, r RTP, settle time.Duration, gate *echo.Gate, clk Clock, log *slog.Logger) (end PumpEnd) {
	defer func() {
		if v := recover(); v != nil {
			end = pumpPanic(v)
		}
	}()
	if log == nil {
		log = slog.Default()
	}
	var progress atomic.Int64 // clock time of the last completed SCO read or write
	progress.Store(clk.Now().UnixNano())
	res := make(chan PumpEnd, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				res <- pumpPanic(v)
			}
		}()
		res <- runPump(ctx, link, r, settle, gate, clk, log, &progress)
	}()

	// The loop's own stall check runs on the goroutine that may be blocked in a
	// link write; this watchdog does not. Without any completed read or write
	// for stallLimit it closes the link and ends the pump.
	wd := clk.NewTicker(watchdogEvery)
	defer wd.Stop()
	for {
		select {
		case e := <-res:
			return e
		case <-ctx.Done():
			select {
			case e := <-res:
				return e
			case <-time.After(unblockWait):
				return PumpCtx // blocked in the link; the caller closes it
			}
		case <-wd.C():
			if clk.Now().Sub(time.Unix(0, progress.Load())) < stallLimit {
				continue
			}
			log.Warn("SCO link made no progress, closing it", "for", stallLimit)
			_ = link.Close()
			select {
			case <-res:
			case <-time.After(unblockWait):
			}
			return PumpSCOStall
		}
	}
}

func runPump(ctx context.Context, link sco.Link, r RTP, settle time.Duration, gate *echo.Gate, clk Clock, log *slog.Logger, progress *atomic.Int64) PumpEnd {
	mtu := link.MTU()
	frames := make(chan []byte, 8)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				readErr <- panicError{v}
			}
		}()
		for {
			buf := make([]byte, mtu)
			n, err := link.Read(buf)
			if err == nil {
				progress.Store(clk.Now().UnixNano())
			}
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

	var failSince, lastWarn time.Time // start of the current RTP failure streak
	// rtpResult digests one RTP send result and reports whether the pump must stop.
	rtpResult := func(op string, err error) bool {
		if err == nil {
			failSince = time.Time{}
			return false
		}
		if errors.Is(err, net.ErrClosed) || errors.Is(err, rtp.ErrNotRunning) {
			return true
		}
		now := clk.Now()
		if failSince.IsZero() {
			failSince = now
		}
		if now.Sub(failSince) >= rtpFailLimit {
			return true
		}
		if lastWarn.IsZero() || now.Sub(lastWarn) >= rtpWarnEvery {
			lastWarn = now
			log.Warn("rtp send failing", "op", op, "for", now.Sub(failSince).Round(time.Millisecond), "error", err)
		}
		return false
	}

	// down writes n bytes of downlink audio to SCO.
	down := func(n int) error {
		out := make([]byte, n&^1)
		pcm := make([]int16, len(out)/2)
		if settled {
			pl.Read(pcm)
			for i, s := range pcm {
				binary.LittleEndian.PutUint16(out[2*i:], uint16(s))
			}
		}
		if gate != nil {
			gate.Reference(pcm) // silence while settling, as written
		}
		_, err := link.Write(out)
		if err == nil {
			progress.Store(clk.Now().UnixNano())
		}
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return PumpCtx
		case err := <-readErr:
			var pe panicError
			if errors.As(err, &pe) {
				return pumpPanic(pe.v)
			}
			return PumpSCOEOF
		case <-settleC:
			settleC = nil
			pl.SetStartup(false)
			settled = true
		case frame := <-frames:
			lastRead = clk.Now()
			// Samples are 16-bit; a stray odd trailing byte cannot be a sample and is dropped.
			for i := 0; i+1 < len(frame); i += 2 {
				pending = append(pending, int16(binary.LittleEndian.Uint16(frame[i:])))
			}
			for len(pending) >= rtpFrameSamples {
				up := pending[:rtpFrameSamples]
				if gate != nil {
					up = gate.Process(up)
				}
				stop := rtpResult("frame", r.WriteFrame(up))
				pending = append(pending[:0], pending[rtpFrameSamples:]...)
				if stop {
					return PumpRTPError
				}
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
				if rtpResult("silence", r.WriteSilence()) {
					return PumpRTPError
				}
			}
		}
	}
}

// panicError carries a panic of the link reader goroutine to the pump loop.
type panicError struct{ v any }

func (p panicError) Error() string { return fmt.Sprint(p.v) }
