package bridge_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/hfp/hfptest"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sco"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

// e2eHooks records the lifecycle events (a bridge.HookSink).
type e2eHooks struct {
	mu     sync.Mutex
	events []string
	ended  []hooks.CallInfo
}

func (h *e2eHooks) Emit(event string, c hooks.CallInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	if event == "call.ended" {
		h.ended = append(h.ended, c)
	}
}

func (h *e2eHooks) count(event string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.events {
		if e == event {
			n++
		}
	}
	return n
}

func (h *e2eHooks) endedCall() (hooks.CallInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ended) == 0 {
		return hooks.CallInfo{}, false
	}
	return h.ended[0], true
}

// oneLink is a sco.Listener that hands out a single link.
type oneLink struct{ links chan sco.Link }

func (l *oneLink) Accept(ctx context.Context) (sco.Link, error) {
	select {
	case k := <-l.links:
		return k, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l *oneLink) Close() error { return nil }

// noDial fails the test if the coordinator dials SCO itself.
type noDial struct{ t *testing.T }

func (d noDial) Dial(ctx context.Context, _, _ string) (sco.Link, error) {
	d.t.Error("coordinator dialed SCO; the phone should have connected")
	<-ctx.Done()
	return nil, ctx.Err()
}

type e2e struct {
	t     *testing.T
	trunk *siptest.Trunk
	ag    *hfptest.AG
	phone sco.Link // the phone's end of the SCO link
	hooks *e2eHooks
}

const (
	e2eCaller = "+380501234567"
	e2eMTU    = 64
)

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// newE2E wires the real hfp.Client, coordinator, SIP adapter and UA, RTP
// session and SCO pipe against a fake AG, a loopback trunk and an RTP echo.
func newE2E(t *testing.T) *e2e {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := siptest.NewTrunk(t, "udp")
	echo := siptest.RTPEcho(t)
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		body := strings.Join([]string{
			"v=0", "o=pbx 1 1 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
			fmt.Sprintf("m=audio %d RTP/AVP 0", echo.Port()), "a=rtpmap:0 PCMU/8000", "",
		}, "\r\n")
		tr.Accept(req, tx, []byte(body))
	})

	cfg := config.Default()
	cfg.SIP.Transport = "udp"
	cfg.SIP.Target = tr.URI("200")
	cfg.NAT.RTPPortMin, cfg.NAT.RTPPortMax = 41000, 42000
	cfg.Phone.Number = "+380441234567"
	cfg.Bluetooth.PhoneAddress = "11:11:11:11:11:11"
	cfg.Calls.CallerIDWaitMs = 500 // +CLIP arrives at once, so this is never waited out
	cfg.Calls.SCOSettleMs = 0
	cfg.Media.JitterMs = 20

	ua, err := sip.NewUA(sip.Options{Transport: "udp", NextHop: tr.HostPort(), UserAgent: "siptooth/e2e", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ua.Close() })

	h := &e2eHooks{}
	adapter := bridge.NewSIPAdapter(bridge.AdapterDeps{UA: ua, Config: cfg, Hooks: nopErrors{}, Logger: logger})
	coord := bridge.New(bridge.Deps{Config: cfg, SIP: adapter, Dialer: noDial{t}, Hooks: h, Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("coordinator did not stop")
		}
	})

	ag, conn := hfptest.New(t, hfptest.DefaultCIND)
	client := hfp.New(conn, logger)
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// pipe a is Remote() 11:11:11:11:11:11, the configured phone.
	link, phone := sco.NewPipe(e2eMTU)
	t.Cleanup(func() { _ = phone.Close() })
	lis := &oneLink{links: make(chan sco.Link, 1)}
	lis.links <- link
	coord.Attach(client, lis, "AA:BB:CC:DD:EE:FF")
	coord.SetAudioRouting(true)
	waitFor(t, "coordinator ready", 3*time.Second, func() bool { return coord.Status().Ready })
	return &e2e{t: t, trunk: tr, ag: ag, phone: phone, hooks: h}
}

type nopErrors struct{}

func (nopErrors) EmitError(string, string) {}

// answerCall rings the phone, waits for the INVITE and takes the call through
// ATA to an active, bridged call.
func (e *e2e) answerCall() {
	t := e.t
	e.ag.Send(fmt.Sprintf("+CIEV: %d,1", hfptest.IdxCallSetup))
	e.ag.Send("RING")
	e.ag.Send(fmt.Sprintf(`+CLIP: "%s",145`, e2eCaller))

	inv, ok := e.trunk.WaitRequest("INVITE", 1, 5*time.Second)
	if !ok {
		t.Fatal("no INVITE at the trunk")
	}
	if from := inv.Header("From"); !strings.Contains(from, e2eCaller) {
		t.Fatalf("From = %q, want the caller %s", from, e2eCaller)
	}
	if !e.ag.WaitCommand("ATA", 5*time.Second) {
		t.Fatalf("no ATA at the AG after 200; got %v", e.ag.Got())
	}
	e.ag.Send(fmt.Sprintf("+CIEV: %d,1", hfptest.IdxCall))
	e.ag.Send(fmt.Sprintf("+CIEV: %d,0", hfptest.IdxCallSetup))
	waitFor(t, "call.connected (SCO up)", 5*time.Second, func() bool { return e.hooks.count("call.connected") > 0 })
}

func (e *e2e) waitEnded() hooks.CallInfo {
	e.t.Helper()
	var info hooks.CallInfo
	waitFor(e.t, "call.ended", 8*time.Second, func() bool {
		var ok bool
		info, ok = e.hooks.endedCall()
		return ok
	})
	return info
}

// TestE2EFarEndHangup: a tone written on the phone's SCO side returns through
// the SIP leg's RTP echo; the far end then hangs up and the phone gets AT+CHUP.
func TestE2EFarEndHangup(t *testing.T) {
	e := newE2E(t)
	e.answerCall()

	sent, got := e.echoTone(400, 1200*time.Millisecond)
	corr, lag := bestCorrelation(sent, got)
	if corr < 0.9 {
		t.Fatalf("echoed tone correlation = %.3f, want > 0.9", corr)
	}
	const win = 4000
	rs, rg := rms(sent[win/2:win/2+win]), rms(got[lag:lag+win])
	if db := 20 * math.Log10(rg/rs); math.Abs(db) > 3 {
		t.Fatalf("echoed level %.0f vs sent %.0f (%.1f dB), want within 3 dB", rg, rs, db)
	}
	t.Logf("tone echoed: correlation %.3f at lag %d samples, level %.0f vs %.0f", corr, lag, rg, rs)

	dialogs := e.trunk.Dialogs()
	if len(dialogs) != 1 {
		t.Fatalf("%d trunk dialogs, want 1", len(dialogs))
	}
	if err := dialogs[0].Bye(context.Background()); err != nil {
		t.Fatalf("trunk BYE: %v", err)
	}
	if !e.ag.WaitCommand("AT+CHUP", 6*time.Second) {
		t.Fatalf("no AT+CHUP after the far end hung up; got %v", e.ag.Got())
	}
	info := e.waitEnded()
	if info.Outcome != bridge.OutcomeCompleted || info.HangupBy != bridge.HangupFarEnd {
		t.Fatalf("outcome %q hangup_by %q, want completed / far_end (error %q)", info.Outcome, info.HangupBy, info.Error)
	}
	if info.Caller == nil || *info.Caller != e2eCaller {
		t.Fatalf("caller = %v, want %s", info.Caller, e2eCaller)
	}
}

// TestE2ECallerHangup: the caller hangs up on the phone (call indicator goes
// to 0); the trunk receives BYE and no AT+CHUP is sent.
func TestE2ECallerHangup(t *testing.T) {
	e := newE2E(t)
	e.answerCall()
	if _, got := e.echoTone(400, 400*time.Millisecond); len(got) == 0 {
		t.Fatal("no audio came back")
	}

	e.ag.Send(fmt.Sprintf("+CIEV: %d,0", hfptest.IdxCall))
	if _, ok := e.trunk.WaitRequest("BYE", 1, 5*time.Second); !ok {
		t.Fatal("trunk got no BYE after the caller hung up")
	}
	info := e.waitEnded()
	if info.Outcome != bridge.OutcomeCompleted || info.HangupBy != bridge.HangupCaller {
		t.Fatalf("outcome %q hangup_by %q, want completed / caller (error %q)", info.Outcome, info.HangupBy, info.Error)
	}
	if n := e.ag.Count("AT+CHUP"); n != 0 {
		t.Fatalf("AT+CHUP sent %d times for a caller hangup", n)
	}
}

// echoTone writes a tone of freq Hz on the phone side of SCO for dur and
// reads the phone side meanwhile (plus a short tail); it returns what was
// sent and what came back, as 8 kHz samples.
func (e *e2e) echoTone(freq float64, dur time.Duration) (sent, got []int16) {
	samplesPerFrame := e2eMTU / 2
	total := int(dur.Seconds()*8000) / samplesPerFrame * samplesPerFrame
	for i := 0; i < total; i++ {
		sent = append(sent, int16(8000*math.Sin(2*math.Pi*freq*float64(i)/8000)))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // reader
		defer wg.Done()
		buf := make([]byte, e2eMTU)
		for {
			n, err := e.phone.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i+1 < n; i += 2 {
				got = append(got, int16(binary.LittleEndian.Uint16(buf[i:])))
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	tick := time.NewTicker(time.Duration(samplesPerFrame) * time.Second / 8000)
	defer tick.Stop()
	for off := 0; off < total; off += samplesPerFrame {
		<-tick.C
		frame := make([]byte, e2eMTU)
		for i, s := range sent[off : off+samplesPerFrame] {
			binary.LittleEndian.PutUint16(frame[2*i:], uint16(s))
		}
		if _, err := e.phone.Write(frame); err != nil {
			e.t.Fatalf("phone SCO write: %v", err)
		}
	}
	time.Sleep(300 * time.Millisecond) // the echo and jitter delay
	// The pump answers every write, so keep it fed until the reader stops.
	close(stop)
	_, _ = e.phone.Write(make([]byte, e2eMTU))
	wg.Wait()
	return sent, got
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

// bestCorrelation is the highest normalized cross-correlation between a
// middle window of sent and got over all lags where the window fits in got,
// and the lag (in samples) where it is highest.
func bestCorrelation(sent, got []int16) (best float64, bestLag int) {
	const win = 4000 // 0.5 s
	if len(sent) < 2*win || len(got) < win {
		return 0, 0
	}
	ref := sent[win/2 : win/2+win]
	for lag := 0; lag+win <= len(got); lag++ {
		seg := got[lag : lag+win]
		var dot, na, nb float64
		for i := range ref {
			a, b := float64(ref[i]), float64(seg[i])
			dot += a * b
			na += a * a
			nb += b * b
		}
		if na == 0 || nb == 0 {
			continue
		}
		if c := dot / math.Sqrt(na*nb); c > best {
			best, bestLag = c, lag
		}
	}
	return best, bestLag
}
