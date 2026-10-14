package rtp

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
)

var lo = netip.MustParseAddr("127.0.0.1")

func listen(t *testing.T) *Session {
	t.Helper()
	s, err := Listen(lo, 30000, 40000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func peer(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func addrOf(c *net.UDPConn) netip.AddrPort { return c.LocalAddr().(*net.UDPAddr).AddrPort() }

func media(remote netip.AddrPort) sdp.Media {
	return sdp.Media{Addr: remote, SendCodec: g711.PCMU, Recv: []g711.Codec{g711.PCMU, g711.PCMA}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func rtpPacket(pt uint8, seq uint16, ts uint32, payload []byte) []byte {
	p := pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: 7},
		Payload: payload,
	}
	b, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func ulaw(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = g711.PCMU.SilenceByte()
	}
	return b
}

func readPeer(t *testing.T, c *net.UDPConn) pionrtp.Packet {
	t.Helper()
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	var p pionrtp.Packet
	if err := p.Unmarshal(buf[:n]); err != nil {
		t.Fatal(err)
	}
	return p
}

func send(t *testing.T, from *net.UDPConn, to netip.AddrPort, b []byte) {
	t.Helper()
	if _, err := from.WriteToUDPAddrPort(b, to); err != nil {
		t.Fatal(err)
	}
}

func TestEvenPortInRange(t *testing.T) {
	for i := 0; i < 20; i++ {
		s, err := Listen(lo, 31001, 31011)
		if err != nil {
			t.Fatal(err)
		}
		p := int(s.LocalAddr().Port())
		s.Close()
		if p%2 != 0 || p < 31002 || p > 31010 {
			t.Fatalf("port %d not even in range", p)
		}
	}
}

func TestEvenPortRetriesInUse(t *testing.T) {
	a, err := Listen(lo, 31100, 31103)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Listen(lo, 31100, 31103) // only two candidates; may need retries
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.LocalAddr() == b.LocalAddr() {
		t.Fatal("same port twice")
	}
}

func TestEarlyPacketsDropped(t *testing.T) {
	s := listen(t)
	p := peer(t)
	send(t, p, s.LocalAddr(), rtpPacket(0, 1, 0, ulaw(160)))
	time.Sleep(50 * time.Millisecond)
	if err := s.Start(media(addrOf(p)), nil, Config{}); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Received != 0 || st.Latched.IsValid() || st.OtherPT != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestQueuedEarlyRTPNotCounted(t *testing.T) {
	s := listen(t)
	p := peer(t)
	for i := 0; i < 5; i++ {
		send(t, p, s.LocalAddr(), rtpPacket(0, uint16(i), uint32(i*160), ulaw(160)))
	}
	time.Sleep(50 * time.Millisecond)
	s.Start(media(addrOf(p)), nil, Config{})
	time.Sleep(50 * time.Millisecond)
	if st := s.Stats(); st.Received != 0 || st.Latched.IsValid() {
		t.Fatalf("early RTP leaked: %+v", st)
	}
	if d := s.Playout().Depth(); d != 0 {
		t.Fatalf("early RTP buffered: %v", d)
	}
}

func TestLatchFirstValidSource(t *testing.T) {
	s := listen(t)
	a, b, c := peer(t), peer(t), peer(t)
	s.Start(media(addrOf(a)), nil, Config{})

	// Before latching, sends go to the SDP address.
	s.WriteSilence()
	readPeer(t, a)

	send(t, b, s.LocalAddr(), rtpPacket(0, 1, 0, ulaw(160)))
	waitFor(t, "latch", func() bool { return s.Stats().Latched == addrOf(b) })
	s.WriteSilence()
	readPeer(t, b)

	send(t, c, s.LocalAddr(), rtpPacket(0, 2, 160, ulaw(160)))
	time.Sleep(50 * time.Millisecond)
	if st := s.Stats(); st.Received != 1 || st.Latched != addrOf(b) {
		t.Fatalf("stats %+v", st)
	}
}

func TestInvalidPacketDoesNotLatch(t *testing.T) {
	s := listen(t)
	a, b := peer(t), peer(t)
	s.Start(media(addrOf(a)), nil, Config{})
	send(t, b, s.LocalAddr(), rtpPacket(13, 1, 0, []byte{0}))
	send(t, b, s.LocalAddr(), []byte("garbage"))
	time.Sleep(50 * time.Millisecond)
	if st := s.Stats(); st.Latched.IsValid() || st.OtherPT != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestUpdateResetsLatch(t *testing.T) {
	s := listen(t)
	a, b, n := peer(t), peer(t), peer(t)
	s.Start(media(addrOf(a)), nil, Config{})
	send(t, b, s.LocalAddr(), rtpPacket(0, 1, 0, ulaw(160)))
	waitFor(t, "latch", func() bool { return s.Stats().Latched == addrOf(b) })

	s.Update(media(addrOf(n)))
	if s.Stats().Latched.IsValid() {
		t.Fatal("latch not reset")
	}
	s.WriteSilence()
	readPeer(t, n)
	send(t, a, s.LocalAddr(), rtpPacket(0, 2, 160, ulaw(160)))
	waitFor(t, "relatch", func() bool { return s.Stats().Latched == addrOf(a) })
}

func TestReceiveBothCodecs(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(1000 + i*10)
	}
	send(t, p, s.LocalAddr(), rtpPacket(0, 1, 0, g711.PCMU.Encode(pcm)))
	send(t, p, s.LocalAddr(), rtpPacket(8, 2, 160, g711.PCMA.Encode(pcm)))
	waitFor(t, "2 packets", func() bool { return s.Stats().Received == 2 })
	s.Playout().SetStartup(false)
	out := make([]int16, 320)
	s.Playout().Read(out)
	for i, want := range append(g711.PCMU.Decode(g711.PCMU.Encode(pcm)), g711.PCMA.Decode(g711.PCMA.Encode(pcm))...) {
		if out[i] != want {
			t.Fatalf("sample %d = %d, want %d", i, out[i], want)
		}
	}
}

func TestOtherPayloadTypesCounted(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	send(t, p, s.LocalAddr(), rtpPacket(13, 1, 0, []byte{0x7f}))
	send(t, p, s.LocalAddr(), rtpPacket(101, 2, 0, []byte{1, 0, 0, 160}))
	waitFor(t, "other pt", func() bool { return s.Stats().OtherPT == 2 })
	if st := s.Stats(); st.Received != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestSharedCounter(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	for i := 0; i < 3; i++ {
		if err := s.WriteSilence(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteFrame(make([]int16, 160)); err != nil {
		t.Fatal(err)
	}
	var pk []pionrtp.Packet
	for i := 0; i < 4; i++ {
		pk = append(pk, readPeer(t, p))
	}
	for i, x := range pk {
		if x.Marker != (i == 0) {
			t.Fatalf("packet %d marker=%v", i, x.Marker)
		}
		if i > 0 {
			if x.SequenceNumber != pk[i-1].SequenceNumber+1 || x.Timestamp != pk[i-1].Timestamp+160 {
				t.Fatalf("packet %d seq/ts not consecutive", i)
			}
		}
		if x.SSRC != pk[0].SSRC || len(x.Payload) != 160 || x.PayloadType != 0 {
			t.Fatalf("packet %d header %+v", i, x.Header)
		}
	}
	if s.Stats().Sent != 4 {
		t.Fatalf("sent = %d", s.Stats().Sent)
	}
	if err := s.WriteFrame(make([]int16, 100)); err == nil {
		t.Fatal("short frame accepted")
	}
}

func srtpPair(t *testing.T) (*Session, *Session) {
	t.Helper()
	ka, kb := make([]byte, 30), make([]byte, 30)
	for i := range ka {
		ka[i], kb[i] = byte(i+1), byte(100+i)
	}
	a, b := listen(t), listen(t)
	ma, mb := media(b.LocalAddr()), media(a.LocalAddr())
	ma.SRTP, ma.RemoteKey = true, kb
	mb.SRTP, mb.RemoteKey = true, ka
	if err := a.Start(ma, ka, Config{}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(mb, kb, Config{}); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestSRTPRoundTrip(t *testing.T) {
	a, b := srtpPair(t)
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(2000 * (i%8 - 4))
	}
	for i := 0; i < 3; i++ {
		if err := a.WriteFrame(pcm); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "3 packets", func() bool { return b.Stats().Received == 3 })
	if b.Stats().SRTPFail != 0 {
		t.Fatalf("stats %+v", b.Stats())
	}
	out := make([]int16, 160)
	b.Playout().Read(out)
	want := g711.PCMU.Decode(g711.PCMU.Encode(pcm))
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("sample %d = %d want %d", i, out[i], want[i])
		}
	}
	// and the other direction
	b.WriteFrame(pcm)
	waitFor(t, "reverse", func() bool { return a.Stats().Received == 1 })
}

func TestSRTPReplayRejected(t *testing.T) {
	ka, kb := make([]byte, 30), make([]byte, 30)
	for i := range ka {
		ka[i], kb[i] = byte(i+1), byte(100+i)
	}
	s := listen(t)
	p := peer(t)
	m := media(addrOf(p))
	m.SRTP, m.RemoteKey = true, ka
	if err := s.Start(m, kb, Config{}); err != nil {
		t.Fatal(err)
	}
	enc, err := srtp.CreateContext(ka[:16], ka[16:], srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := enc.EncryptRTP(nil, rtpPacket(0, 1, 0, ulaw(160)), nil)
	if err != nil {
		t.Fatal(err)
	}
	send(t, p, s.LocalAddr(), pkt)
	waitFor(t, "first", func() bool { return s.Stats().Received == 1 })
	send(t, p, s.LocalAddr(), pkt)
	waitFor(t, "replay", func() bool { return s.Stats().SRTPFail == 1 })
	if s.Stats().Received != 1 || s.Playout().Depth() != 20*time.Millisecond {
		t.Fatalf("replayed packet played: %+v", s.Stats())
	}
	// A forged (unauthenticated) packet is also rejected.
	send(t, p, s.LocalAddr(), rtpPacket(0, 2, 160, ulaw(160)))
	waitFor(t, "forged", func() bool { return s.Stats().SRTPFail == 2 })
}

// stunResponse builds a Binding success response for req mapping to ap.
func stunResponse(req []byte, ap netip.AddrPort) []byte {
	r := make([]byte, 32)
	binary.BigEndian.PutUint16(r[0:], 0x0101)
	binary.BigEndian.PutUint16(r[2:], 12)
	copy(r[4:], req[4:20])
	binary.BigEndian.PutUint16(r[20:], 0x0020)
	binary.BigEndian.PutUint16(r[22:], 8)
	r[25] = 1
	binary.BigEndian.PutUint16(r[26:], ap.Port()^0x2112)
	ip := ap.Addr().As4()
	cookie := []byte{0x21, 0x12, 0xa4, 0x42}
	for i := range ip {
		r[28+i] = ip[i] ^ cookie[i]
	}
	return r
}

// stunServer answers every Binding Request with mapped(); nil mapped = silent.
func stunServer(t *testing.T, mapped func() netip.AddrPort) (*net.UDPConn, chan struct{}) {
	t.Helper()
	c := peer(t)
	reqs := make(chan struct{}, 100)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			reqs <- struct{}{}
			if m := mapped(); m.IsValid() {
				c.WriteToUDPAddrPort(stunResponse(buf[:n], m), from)
			}
		}
	}()
	return c, reqs
}

func TestSTUNQueryBeforeStart(t *testing.T) {
	s := listen(t)
	want := netip.MustParseAddrPort("203.0.113.5:4444")
	srv, _ := stunServer(t, func() netip.AddrPort { return want })
	got, err := s.QuerySTUN(addrOf(srv).String(), time.Second, 2)
	if err != nil || got != want {
		t.Fatalf("got %v, %v", got, err)
	}
	if st := s.Stats(); st.Received != 0 {
		t.Fatalf("stats %+v", st)
	}
	s.Start(media(addrOf(srv)), nil, Config{})
	if _, err := s.QuerySTUN(addrOf(srv).String(), 50*time.Millisecond, 1); err == nil {
		t.Fatal("QuerySTUN after Start must fail")
	}
}

func TestSTUNQueryTimeout(t *testing.T) {
	s := listen(t)
	srv, _ := stunServer(t, func() netip.AddrPort { return netip.AddrPort{} })
	start := time.Now()
	if _, err := s.QuerySTUN(addrOf(srv).String(), 50*time.Millisecond, 2); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("did not wait for all attempts")
	}
}

func TestSTUNKeepaliveStopsAtStart(t *testing.T) {
	s := listen(t)
	m1 := netip.MustParseAddrPort("203.0.113.5:4444")
	m2 := netip.MustParseAddrPort("203.0.113.5:5555")
	cur := make(chan netip.AddrPort, 1)
	cur <- m1
	var latest = func() netip.AddrPort { v := <-cur; cur <- v; return v }
	srv, reqs := stunServer(t, latest)
	if _, err := s.QuerySTUN(addrOf(srv).String(), time.Second, 1); err != nil {
		t.Fatal(err)
	}
	<-reqs
	changed := make(chan netip.AddrPort, 4)
	s.StartSTUNKeepalive(addrOf(srv).String(), 20*time.Millisecond, func(a netip.AddrPort) { changed <- a })
	<-reqs
	<-cur
	cur <- m2
	select {
	case a := <-changed:
		if a != m2 {
			t.Fatalf("changed to %v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no change notification")
	}
	s.Start(media(addrOf(srv)), nil, Config{})
	for len(reqs) > 0 {
		<-reqs
	}
	time.Sleep(100 * time.Millisecond)
	if len(reqs) != 0 {
		t.Fatal("keepalive kept sending after Start")
	}
}

func TestSTUNResponseCrossingStart(t *testing.T) {
	s := listen(t)
	srv, reqs := stunServer(t, func() netip.AddrPort { return netip.AddrPort{} })
	errc := make(chan error, 1)
	go func() {
		_, err := s.QuerySTUN(addrOf(srv).String(), 2*time.Second, 1)
		errc <- err
	}()
	<-reqs
	s.Start(media(addrOf(srv)), nil, Config{})
	// A response arriving after Start is ignored (and never counted as RTP).
	var fake [20]byte
	fake[0], fake[1] = 0x01, 0x01
	send(t, srv, s.LocalAddr(), fake[:])
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("query succeeded across Start")
		}
	case <-time.After(time.Second):
		t.Fatal("query did not abort at Start")
	}
	time.Sleep(30 * time.Millisecond)
	if st := s.Stats(); st.Received != 0 || st.OtherPT != 0 || st.Latched.IsValid() {
		t.Fatalf("stats %+v", st)
	}
}

func TestSTUNIgnoredAfterStart(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	var msg [20]byte
	msg[1] = 1
	send(t, p, s.LocalAddr(), msg[:])
	time.Sleep(50 * time.Millisecond)
	if st := s.Stats(); st.Received != 0 || st.Latched.IsValid() || st.OtherPT != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestMaxGap(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	send(t, p, s.LocalAddr(), rtpPacket(0, 1, 0, ulaw(160)))
	waitFor(t, "first", func() bool { return s.Stats().Received == 1 })
	if s.Stats().MaxGap != 0 {
		t.Fatal("gap before second packet")
	}
	time.Sleep(120 * time.Millisecond)
	send(t, p, s.LocalAddr(), rtpPacket(0, 2, 160, ulaw(160)))
	waitFor(t, "second", func() bool { return s.Stats().Received == 2 })
	if g := s.Stats().MaxGap; g < 100*time.Millisecond || g > time.Second {
		t.Fatalf("MaxGap = %v", g)
	}
}

func TestLostAndReorder(t *testing.T) {
	s := listen(t)
	p := peer(t)
	s.Start(media(addrOf(p)), nil, Config{})
	for _, seq := range []uint16{1, 2, 5, 3} {
		send(t, p, s.LocalAddr(), rtpPacket(0, seq, uint32(seq)*160, ulaw(160)))
	}
	waitFor(t, "4", func() bool { return s.Stats().Received == 4 })
	if l := s.Stats().Lost; l != 1 {
		t.Fatalf("lost = %d, want 1 (seq 4)", l)
	}
}

func TestErrOnClosedSocket(t *testing.T) {
	s := listen(t)
	s.conn.Close() // fatal read error underneath the session
	select {
	case err, ok := <-s.Err():
		if !ok || err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(time.Second):
		t.Fatal("no error reported")
	}
}

func TestCloseIsQuiet(t *testing.T) {
	s := listen(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err, ok := <-s.Err(); ok {
		t.Fatalf("unexpected error %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSilence(); err == nil {
		t.Fatal("write after close succeeded")
	}
}

func TestConcurrentUse(t *testing.T) {
	a, b := srtpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pcm := make([]int16, 160)
		for i := 0; i < 100; i++ {
			a.WriteFrame(pcm)
		}
	}()
	go func() {
		out := make([]int16, 30)
		for i := 0; i < 500; i++ {
			b.Playout().Read(out)
			b.Stats()
		}
	}()
	<-done
	waitFor(t, "recv", func() bool { return b.Stats().Received > 50 })
}
