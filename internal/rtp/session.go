// Package rtp is the per-call RTP session (spec 6.1, 6.3, 6.5): it owns the
// UDP socket and its only reader, latches onto the far end's source, handles
// SRTP, and feeds received audio into a timestamp-based playout buffer.
package rtp

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	pionrtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/stun"
)

const (
	frameSamples = 160
	maxPacket    = 2048
	writeTimeout = 100 * time.Millisecond
)

// Session is one call's RTP socket.
type Session struct {
	conn  *net.UDPConn
	local netip.AddrPort

	errCh      chan error
	readerDone chan struct{}
	startedCh  chan struct{} // closed by Start
	barrierCh  chan struct{} // closed when the reader sees Start's marker
	stopKACh   chan struct{} // closed at Start or Close

	sendMu      sync.Mutex // serialises packet construction, seq/ts and the socket write
	closeDone   chan struct{}
	closeErr    error
	mu          sync.Mutex
	started     bool
	starting    bool
	barrier     []byte
	barrierOnce sync.Once
	closed      bool
	kaOnce      sync.Once
	kaDone      chan struct{}
	pending     map[[12]byte]chan []byte
	lastSTUN    netip.AddrPort

	remote  netip.AddrPort // SDP address
	latched netip.AddrPort
	sendPT  uint8
	recvPT  map[uint8]g711.Codec
	srtpOn  bool
	sendCtx *srtp.Context
	recvCtx *srtp.Context
	remKey  []byte

	ssrc     uint32 // send state below is guarded by sendMu
	seq      uint16
	ts       uint32
	sentAny  bool
	pl       *Playout
	st       Stats
	sent     levels // level of the frames sent by WriteFrame (s.mu)
	hiSeq    uint16
	missing  map[uint16]struct{}
	haveSeq  bool
	ssrcRecv uint32
	lastAcc  time.Time
}

// Listen binds an even UDP port, random within [portMin, portMax], retrying
// when a port is in use, and starts the socket's only reader. Until Start the
// reader serves STUN queries and drops everything else.
func Listen(bind netip.Addr, portMin, portMax int) (*Session, error) {
	lo := (portMin + 1) &^ 1
	hi := portMax &^ 1
	if portMin < 1 || portMax > 65535 || lo > hi {
		return nil, fmt.Errorf("rtp: no even port in range %d-%d", portMin, portMax)
	}
	n := (hi-lo)/2 + 1
	var conn *net.UDPConn
	var lastErr error
	for _, k := range rand.Perm(n)[:min(n, 200)] {
		port := lo + 2*k
		c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(bind, uint16(port))))
		if err == nil {
			conn = c
			break
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("rtp: listen: %w", err)
		}
	}
	if conn == nil {
		return nil, fmt.Errorf("rtp: no free even port in %d-%d: %w", portMin, portMax, lastErr)
	}
	s := &Session{
		conn:       conn,
		local:      conn.LocalAddr().(*net.UDPAddr).AddrPort(),
		errCh:      make(chan error, 1),
		readerDone: make(chan struct{}),
		startedCh:  make(chan struct{}),
		barrierCh:  make(chan struct{}),
		stopKACh:   make(chan struct{}),
		closeDone:  make(chan struct{}),
		pending:    map[[12]byte]chan []byte{},
		ssrc:       rand.Uint32(),
		seq:        uint16(rand.Uint32()),
		ts:         rand.Uint32(),
	}
	go s.readLoop()
	return s, nil
}

func (s *Session) LocalAddr() netip.AddrPort { return s.local }

// ErrNotRunning is returned by the send methods before Start and after Close.
var ErrNotRunning = errors.New("rtp: session not running")

// Err delivers a fatal socket read error (not per-packet failures). It is
// closed when the reader exits.
func (s *Session) Err() <-chan error { return s.errCh }

// Playout returns the playout buffer; nil until Start.
func (s *Session) Playout() *Playout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pl
}

// Start begins media: packets are now accepted and latched. localKey is the
// 30-byte key we offered (used to send); m.RemoteKey is used to receive. The
// playout buffer starts in startup mode.
func (s *Session) Start(m sdp.Media, localKey []byte, cfg Config) error {
	var sendCtx, recvCtx *srtp.Context
	if m.SRTP {
		var err error
		if sendCtx, err = newSendContext(localKey); err != nil {
			return err
		}
		if recvCtx, err = newRecvContext(m.RemoteKey); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("rtp: session closed")
	}
	if s.started || s.starting {
		s.mu.Unlock()
		return errors.New("rtp: already started")
	}
	s.starting = true
	var token [16]byte
	copy(token[:], "siptooth-barrier")
	for i := 8; i < 16; i++ {
		token[i] = byte(rand.Uint32())
	}
	s.barrier = token[:]
	s.mu.Unlock()

	// Datagrams queued in the socket before this point must not be accepted.
	// The reader drops everything until Start activates; the socket queue is
	// FIFO, so once the reader has seen a marker sent to ourselves now, every
	// earlier datagram has been read and dropped.
	if err := s.writeUDP(token[:], s.selfAddr()); err == nil {
		select {
		case <-s.barrierCh:
		case <-s.readerDone: // closed or fatal read error
		case <-time.After(time.Second): // marker lost: best effort
		}
	} else {
		// Deliberate fallback: give the reader a moment to drain the queue.
		slog.Warn("rtp: start barrier write failed; draining by delay", "err", err)
		select {
		case <-s.readerDone:
		case <-time.After(50 * time.Millisecond):
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("rtp: session closed")
	}
	if m.SRTP {
		s.sendCtx, s.recvCtx = sendCtx, recvCtx
		s.remKey = append([]byte(nil), m.RemoteKey...)
	}
	s.srtpOn = m.SRTP
	s.applyMedia(m)
	s.pl = NewPlayout(cfg)
	s.pl.SetStartup(true)
	s.started = true
	close(s.startedCh)
	s.mu.Unlock()

	s.stopKeepalive()
	return nil
}

// writeUDP sends one datagram under a fresh write deadline, so a stuck socket
// can never block a caller (or Close) for long.
func (s *Session) writeUDP(b []byte, dst netip.AddrPort) error {
	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := s.conn.WriteToUDPAddrPort(b, dst)
	return err
}

// selfAddr is the address a datagram can be sent to to reach our own socket.
func (s *Session) selfAddr() netip.AddrPort {
	a := s.local
	if a.Addr().IsUnspecified() {
		if a.Addr().Is4() {
			return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), a.Port())
		}
		return netip.AddrPortFrom(netip.IPv6Loopback(), a.Port())
	}
	return a
}

func (s *Session) applyMedia(m sdp.Media) {
	s.remote = m.Addr
	s.latched = netip.AddrPort{}
	s.st.Latched = netip.AddrPort{}
	s.sendPT = uint8(m.SendCodec)
	s.recvPT = map[uint8]g711.Codec{}
	for _, c := range m.Recv {
		s.recvPT[uint8(c)] = c
	}
}

// Update applies a re-INVITE's media description and resets the latch to the
// new SDP address.
func (s *Session) Update(m sdp.Media) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed {
		return
	}
	if s.srtpOn && m.SRTP && len(m.RemoteKey) == srtpKeyLen && string(m.RemoteKey) != string(s.remKey) {
		if ctx, err := newRecvContext(m.RemoteKey); err == nil {
			s.recvCtx = ctx
			s.remKey = append([]byte(nil), m.RemoteKey...)
		}
	}
	s.applyMedia(m)
}

// WriteFrame sends one 20 ms frame (160 samples) with the send codec.
func (s *Session) WriteFrame(pcm []int16) error {
	if len(pcm) != frameSamples {
		return fmt.Errorf("rtp: frame must be %d samples, got %d", frameSamples, len(pcm))
	}
	s.mu.Lock()
	s.sent.add(pcm)
	s.mu.Unlock()
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendLocked(func(c g711.Codec) []byte { return c.Encode(pcm) })
}

// WriteSilence sends one 20 ms silence packet with the send codec.
func (s *Session) WriteSilence() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendLocked(func(c g711.Codec) []byte {
		payload := make([]byte, frameSamples)
		b := c.SilenceByte()
		for i := range payload {
			payload[i] = b
		}
		return payload
	})
}

// sendLocked transmits one packet; s.sendMu must be held. s.mu is held only to
// snapshot state, never across the socket write, so Close cannot block on a
// stuck write.
func (s *Session) sendLocked(payload func(g711.Codec) []byte) error {
	s.mu.Lock()
	if !s.started || s.closed {
		s.mu.Unlock()
		return ErrNotRunning
	}
	dst := s.remote
	if s.latched.IsValid() {
		dst = s.latched
	}
	pt, srtpOn, ctx := s.sendPT, s.srtpOn, s.sendCtx
	s.mu.Unlock()

	pkt := pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			Marker:         !s.sentAny,
			PayloadType:    pt,
			SequenceNumber: s.seq,
			Timestamp:      s.ts,
			SSRC:           s.ssrc,
		},
		Payload: payload(g711.Codec(pt)),
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtp: marshal: %w", err)
	}
	if srtpOn {
		if raw, err = ctx.EncryptRTP(nil, raw, nil); err != nil {
			return fmt.Errorf("rtp: srtp encrypt: %w", err)
		}
	}
	if err := s.writeUDP(raw, dst); err != nil {
		return fmt.Errorf("rtp: send: %w", err)
	}
	s.sentAny = true
	s.seq++
	s.ts += frameSamples
	s.mu.Lock()
	s.st.Sent++
	s.mu.Unlock()
	return nil
}

// Stats returns a snapshot of the counters.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.SentSpeechP50DB, st.SentSpeechP95DB, st.SentSpeechFrames = s.sent.summary()
	if s.pl != nil {
		c := s.pl.counters()
		st.Late, st.Duplicate, st.Overflow, st.DiscardedSilence = c.late, c.duplicate, c.overflow, c.discarded
	}
	return st
}

// Close stops the reader and keepalive and closes the socket. Idempotent;
// every caller returns only after cleanup has completed.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.closeDone
		return s.closeErr
	}
	s.closed = true
	s.mu.Unlock()
	// Close the socket first so blocked reads and writes return, then join.
	s.closeErr = s.conn.Close()
	s.stopKeepalive()
	<-s.readerDone
	close(s.closeDone)
	return s.closeErr
}

func (s *Session) readLoop() {
	defer close(s.readerDone)
	defer close(s.errCh)
	buf := make([]byte, maxPacket)
	for {
		n, src, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				s.errCh <- fmt.Errorf("rtp: read: %w", err)
			}
			return
		}
		s.handle(buf[:n], netip.AddrPortFrom(src.Addr().Unmap(), src.Port()))
	}
}

func (s *Session) handle(b []byte, src netip.AddrPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		if s.barrier != nil && string(b) == string(s.barrier) {
			s.barrierOnce.Do(func() { close(s.barrierCh) })
			return
		}
		if stun.IsSTUN(b) && len(b) >= 20 {
			var id [12]byte
			copy(id[:], b[8:20])
			if ch, ok := s.pending[id]; ok {
				select {
				case ch <- append([]byte(nil), b...):
				default:
				}
			}
		}
		return
	}
	if stun.IsSTUN(b) {
		return
	}
	if s.latched.IsValid() && src != s.latched {
		return
	}
	var h pionrtp.Header
	if _, err := h.Unmarshal(b); err != nil || h.Version != 2 {
		return
	}
	codec, ok := s.recvPT[h.PayloadType]
	if !ok {
		s.st.OtherPT++
		return
	}
	pkt := b
	if s.srtpOn {
		dec, err := s.recvCtx.DecryptRTP(nil, b, nil)
		if err != nil {
			s.st.SRTPFail++
			return
		}
		pkt = dec
	}
	var p pionrtp.Packet
	if err := p.Unmarshal(pkt); err != nil {
		return
	}

	now := time.Now()
	if !s.latched.IsValid() {
		s.latched = src
		s.st.Latched = src
	}
	if !s.lastAcc.IsZero() {
		s.st.MaxGap = max(s.st.MaxGap, now.Sub(s.lastAcc))
	}
	s.lastAcc = now
	s.st.LastRx = now
	s.st.Received++
	s.trackSeq(p.SSRC, p.SequenceNumber)
	if len(p.Payload) > 0 {
		s.pl.Push(p.Timestamp, p.SSRC, codec.Decode(p.Payload))
	}
}

// trackSeq counts sequence gaps as lost. Only sequence numbers recorded as
// missing within the last 64 of the current SSRC are un-counted when a
// reordered packet fills them.
func (s *Session) trackSeq(ssrc uint32, seq uint16) {
	if !s.haveSeq || ssrc != s.ssrcRecv {
		s.haveSeq, s.ssrcRecv, s.hiSeq = true, ssrc, seq
		s.missing = map[uint16]struct{}{}
		return
	}
	d := int(int16(seq - s.hiSeq))
	if d > 0 {
		if d < 1000 {
			s.st.Lost += uint64(d - 1)
			for m := s.hiSeq + 1; m != seq; m++ {
				s.missing[m] = struct{}{}
			}
		}
		s.hiSeq = seq
		for m := range s.missing {
			if s.hiSeq-m >= 64 {
				delete(s.missing, m)
			}
		}
		return
	}
	if _, ok := s.missing[seq]; ok {
		delete(s.missing, seq)
		s.st.Lost--
	}
}

// QuerySTUN sends Binding Requests from the RTP socket and returns the mapped
// address. Only valid before Start.
func (s *Session) QuerySTUN(server string, timeout time.Duration, attempts int) (netip.AddrPort, error) {
	dst, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("stun: resolve %s: %w", server, err)
	}
	if attempts < 1 {
		attempts = 1
	}
	lastErr := fmt.Errorf("stun: %s: timeout", server)
	for i := 0; i < attempts; i++ {
		ap, err := s.stunOnce(dst, timeout)
		if err == nil {
			return ap, nil
		}
		if errors.Is(err, errSTUNStopped) {
			return netip.AddrPort{}, err
		}
		lastErr = err
	}
	return netip.AddrPort{}, lastErr
}

var errSTUNStopped = errors.New("stun: session started or closed")

func (s *Session) stunOnce(dst *net.UDPAddr, timeout time.Duration) (netip.AddrPort, error) {
	txID, msg := stun.NewBindingRequest()
	ch := make(chan []byte, 4)
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return netip.AddrPort{}, errSTUNStopped
	}
	s.pending[txID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, txID)
		s.mu.Unlock()
	}()

	if err := s.writeUDP(msg, dst.AddrPort()); err != nil {
		return netip.AddrPort{}, fmt.Errorf("stun: send to %s: %w", dst, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case b := <-ch:
			ap, err := stun.ParseBindingResponse(b, txID)
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.lastSTUN = ap
			s.mu.Unlock()
			return ap, nil
		case <-timer.C:
			return netip.AddrPort{}, fmt.Errorf("stun: %s: timeout", dst)
		case <-s.startedCh:
			return netip.AddrPort{}, errSTUNStopped
		case <-s.stopKACh:
			return netip.AddrPort{}, errSTUNStopped
		}
	}
}

// StartSTUNKeepalive re-sends a Binding Request every interval until Start or
// Close, calling onChange (from its own goroutine) when the mapping differs
// from the last one seen.
func (s *Session) StartSTUNKeepalive(server string, every time.Duration, onChange func(netip.AddrPort)) {
	dst, err := net.ResolveUDPAddr("udp", server)
	if err != nil || every <= 0 {
		return
	}
	s.mu.Lock()
	if s.started || s.closed || s.kaDone != nil {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	s.kaDone = done
	s.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-s.stopKACh:
				return
			case <-s.startedCh:
				return
			case <-t.C:
			}
			s.mu.Lock()
			prev := s.lastSTUN
			s.mu.Unlock()
			ap, err := s.stunOnce(dst, min(every, 2*time.Second))
			if err != nil {
				if errors.Is(err, errSTUNStopped) {
					return
				}
				continue
			}
			if prev.IsValid() && ap != prev && onChange != nil {
				go onChange(ap) // never block the goroutine Start/Close join
			}
		}
	}()
}

// stopKeepalive stops the keepalive goroutine and waits for it.
func (s *Session) stopKeepalive() {
	s.kaOnce.Do(func() { close(s.stopKACh) })
	s.mu.Lock()
	done := s.kaDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}
