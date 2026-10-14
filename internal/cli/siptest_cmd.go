package cli

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/buildinfo"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sipaddr"
)

func init() {
	register(command{"sip", "sip test [--caller N] [--seconds 20] [--tone] [--wait-remote-hangup]: a real SIP call to the target without Bluetooth", runSIP})
}

const (
	// toneAmplitude is 1 kHz at -20 dBFS (32768 * 0.1).
	toneAmplitude = 3277
	frameSamples  = 160
	frameEvery    = 20 * time.Millisecond
	// waitHangupMax bounds --wait-remote-hangup.
	waitHangupMax = 300 * time.Second
	// registerWait bounds waiting for the first registration answer.
	registerWait = 15 * time.Second
	// byeTimeout and unregisterTimeout bound the teardown.
	byeTimeout        = 5 * time.Second
	unregisterTimeout = 5 * time.Second
)

// lockedWriter serializes writes from the SIP callbacks and the main loop.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// sipTestTLS is replaced by tests (the fake trunk's CA); nil means system roots.
var sipTestTLS *tls.Config

func runSIP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "test" {
		fmt.Fprintln(stderr, "usage: siptooth sip test [--caller N] [--seconds 20] [--tone] [--wait-remote-hangup] [--config PATH]")
		return ExitUsage
	}
	fs := flag.NewFlagSet("sip test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	caller := fs.String("caller", "+10000000000", "caller number fed to {caller}")
	seconds := fs.Int("seconds", 20, "how long to stay in the call")
	tone := fs.Bool("tone", false, "send a 1 kHz tone at -20 dBFS instead of silence")
	waitHangup := fs.Bool("wait-remote-hangup", false, "keep the call up (max 300 s) until the far end ends it")
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if *seconds < 1 {
		fmt.Fprintln(stderr, "--seconds must be at least 1")
		return ExitUsage
	}
	level, ok := logLevelFromEnv()
	if !ok {
		fmt.Fprintf(stderr, "invalid %s %q (debug, info, warn or error)\n", logLevelVar, os.Getenv(logLevelVar))
		return ExitUsage
	}
	cfg, password, ok := loadChecked(*cfgPath, stderr)
	if !ok {
		return ExitUsage
	}
	logger := newLogger(&lockedWriter{w: stderr}, level)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := &lockedWriter{w: stdout}
	t := &sipTest{
		cfg: cfg, password: password, out: out, log: logger,
		caller: *caller, seconds: *seconds, tone: *tone, waitHangup: *waitHangup,
	}
	return t.run(ctx)
}

type sipTest struct {
	cfg        config.Config
	password   string
	out        io.Writer
	log        *slog.Logger
	caller     string
	seconds    int
	tone       bool
	waitHangup bool
}

func (t *sipTest) printf(format string, a ...any) { fmt.Fprintf(t.out, format+"\n", a...) }

// report is the final block.
type report struct {
	answered bool
	status   string
	stats    mediaTotals
	rmsDB    string
	endedBy  string
}

type mediaTotals struct {
	sent, recv, lost, late uint64
	maxGap                 time.Duration
	latched                string
}

func (t *sipTest) run(ctx context.Context) int {
	cfg := t.cfg
	// The test never competes with a running daemon: its own ephemeral SIP
	// port for every transport, its own RTP socket.
	cfg.SIP.LocalPort = 0

	nextHop, err := sipaddr.NextHop(cfg)
	if err != nil {
		t.printf("error: %v", err)
		return ExitError
	}
	host, _, err := net.SplitHostPort(nextHop)
	if err != nil {
		t.printf("error: next hop %q: %v", nextHop, err)
		return ExitError
	}
	ua, err := sip.NewUA(sip.Options{
		Transport: cfg.SIP.Transport, NextHop: nextHop, LocalPort: 0,
		Username: cfg.SIP.Username, Password: t.password,
		UserAgent: "siptooth/" + buildinfo.Version, Logger: t.log, TLSConfig: sipTestTLS,
	})
	if err != nil {
		t.printf("error: %v", err)
		return ExitError
	}
	defer ua.Close()

	var reg atomic.Pointer[sip.Registration]
	if cfg.SIP.Register {
		r, registered := t.register(ctx, ua, cfg)
		reg.Store(r)
		defer func() {
			uctx, cancel := context.WithTimeout(context.Background(), unregisterTimeout)
			defer cancel()
			err := r.Unregister(uctx)
			switch {
			case err != nil:
				t.printf("unregister failed: %v", err)
			case registered:
				t.printf("unregistered")
			}
		}()
	}

	adapter := bridge.NewSIPAdapter(bridge.AdapterDeps{
		UA: ua, Config: cfg, Logger: t.log,
		Registration: func() (sip.RegState, bool) {
			r := reg.Load()
			if r == nil {
				return sip.RegState{}, false
			}
			return r.State(), true
		},
	})
	prep, err := adapter.Prepare(ctx, host)
	if err != nil {
		t.printf("error: prepare: %v", err)
		return ExitError
	}
	media := prep.Media
	defer media.Close()
	dec := prep.NAT
	t.printf("nat: mode=%s local=%s advertised=%s", dec.Mode, dec.Local, dec.Media)
	if dec.STUNServer != "" {
		t.printf("nat: stun server %s", dec.STUNServer)
	}
	if dec.STUNFailed {
		t.printf("nat: STUN failed")
	}
	if dec.Warning != "" {
		t.printf("nat: warning: %s", dec.Warning)
	}
	t.printf("contact: %s (source: %s)", prep.Contact, prep.ContactSource)
	t.probeOptions(ctx, ua, host)

	req, err := t.dialRequest(cfg, prep, nextHop)
	if err != nil {
		t.printf("error: %v", err)
		return ExitError
	}
	t.printf("dialing %s (next hop %s, %s)", req.Target, nextHop, cfg.SIP.Transport)
	att, err := adapter.Start(req)
	if err != nil {
		t.printf("error: start invite: %v", err)
		return ExitError
	}
	rep := report{status: "no response"}
	var res sip.DialResult
	dialTimeout := time.NewTimer(time.Duration(cfg.SIP.DialTimeoutSeconds) * time.Second)
	defer dialTimeout.Stop()
	select {
	case res = <-att.Result():
	case <-dialTimeout.C:
		att.Abandon()
		res = <-att.Result()
		t.printf("no answer within %d s, cancelled", cfg.SIP.DialTimeoutSeconds)
	case <-ctx.Done():
		att.Abandon()
		res = <-att.Result()
		t.printf("interrupted, cancelled")
	}
	if res.Call == nil {
		if res.Status != 0 {
			rep.status = fmt.Sprintf("%d %s", res.Status, res.Reason)
		} else if res.Reason != "" {
			rep.status = res.Reason
		}
		if res.Err != nil {
			t.printf("failed: %s: %v", res.Cause, res.Err)
		} else {
			t.printf("failed: %s", res.Cause)
		}
		t.finalReport(rep, false)
		return ExitError
	}
	call := res.Call
	rep.answered = true
	st, rsn := call.LastStatus()
	rep.status = fmt.Sprintf("%d %s", st, rsn)
	remote := call.Remote()
	t.printf("answered: call-id %s", call.SIPCallID())
	t.printf("codec: %s, srtp: %s, remote media %s", remote.SendCodec, onOff(remote.SRTP), remote.Addr)
	if err := media.Start(remote, prep.LocalKey); err != nil {
		t.printf("error: media: %v", err)
		hangup(call)
		rep.endedBy = "local hangup"
		t.finalReport(rep, false)
		return ExitError
	}
	rep.endedBy, rep.rmsDB = t.talk(ctx, call, media)
	if rep.endedBy == "local hangup" || rep.endedBy == "media stopped" || rep.endedBy == "wait limit reached" {
		hangup(call)
	}
	s := media.Stats()
	rep.stats = mediaTotals{sent: s.Sent, recv: s.Received, lost: s.Lost, late: s.Late, maxGap: s.MaxGap}
	if s.Latched.IsValid() {
		rep.stats.latched = s.Latched.String()
	}
	t.finalReport(rep, true)
	if s.Received >= 1 {
		return ExitOK
	}
	return ExitError
}

// probeOptions shows what the far end observes of this flow: the top Via's
// received/rport of an OPTIONS answer. It runs whatever the adapter used for the Contact.
func (t *sipTest) probeOptions(ctx context.Context, ua *sip.UA, host string) {
	target := "sip:" + host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		target = "sip:[" + host + "]"
	}
	ap, reason := ua.DiscoverContactReason(ctx, target)
	if reason != "" {
		t.printf("options: %s", reason)
		return
	}
	t.printf("options: received=%s rport=%d", ap.Addr(), ap.Port())
}

func hangup(call sip.Call) {
	hctx, cancel := context.WithTimeout(context.Background(), byeTimeout)
	defer cancel()
	_ = call.Hangup(hctx)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// register registers the test's own Contact and waits (bounded) for the
// registered state, which the registration publishes only once the Contact is
// the registrar-observed flow address; it prints that final Contact.
func (t *sipTest) register(ctx context.Context, ua *sip.UA, cfg config.Config) (reg *sip.Registration, ok bool) {
	states := make(chan sip.RegState, 16)
	r := ua.Register(context.Background(), cfg.SIP.RegistrationAOR(), cfg.SIP.RegistrarHost(),
		time.Duration(cfg.SIP.RegisterExpiresSeconds)*time.Second, func(s sip.RegState) {
			select {
			case states <- s:
			default:
			}
		})
	timeout := time.NewTimer(registerWait)
	defer timeout.Stop()
	for {
		select {
		case s := <-states:
			switch {
			case s.Registered:
				t.printf("register: ok, contact %s", s.Contact)
				return r, true
			case s.Err != nil:
				t.printf("register: failed: %v (continuing)", s.Err)
				return r, false
			}
		case <-timeout.C:
			t.printf("register: no answer within %s (continuing)", registerWait)
			return r, false
		case <-ctx.Done():
			return r, false
		}
	}
}

// dialRequest expands the identity placeholders as the daemon does.
func (t *sipTest) dialRequest(cfg config.Config, prep bridge.Prepared, nextHop string) (sip.DialRequest, error) {
	who, withheld := placeholder.CleanCaller(t.caller, 0)
	if withheld {
		who = cfg.SIP.AnonymousUser
	}
	v := placeholder.Values{Caller: who, Number: cfg.Phone.Number, CallID: newTestCallID()}
	req := sip.DialRequest{
		Contact: prep.Contact, Offer: prep.Offer, NextHop: nextHop, Transport: cfg.SIP.Transport,
		OnResponse: func(status int, reason string) { t.printf("<- %d %s", status, reason) },
	}
	var err error
	if req.Target, err = placeholder.ExpandURI(cfg.SIP.Target, v); err != nil {
		return req, fmt.Errorf("sip.target: %w", err)
	}
	if req.From, err = placeholder.ExpandURI(cfg.SIP.FromTemplate(), v); err != nil {
		return req, fmt.Errorf("sip.from: %w", err)
	}
	if cfg.SIP.DisplayName != "" {
		req.DisplayName = placeholder.ExpandDisplayName(cfg.SIP.DisplayName, v)
	}
	if len(cfg.SIP.Headers) > 0 {
		req.Headers = make(map[string]string, len(cfg.SIP.Headers))
		for k, tmpl := range cfg.SIP.Headers {
			if req.Headers[k], err = placeholder.ExpandHeader(tmpl, v); err != nil {
				return req, fmt.Errorf("header %s: %w", k, err)
			}
		}
	}
	if req.Offer.LocalKey == nil && len(prep.LocalKey) > 0 {
		req.Offer.LocalKey = prep.LocalKey
	}
	return req, nil
}

func newTestCallID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func dbfs(sumSq float64, n int) string {
	if n == 0 || sumSq == 0 {
		return "-inf"
	}
	return fmt.Sprintf("%.1f", 20*math.Log10(math.Sqrt(sumSq/float64(n))/32768))
}

// talk runs the media loop until the call ends one way or another. It returns
// the end reason and the received level over the whole call.
func (t *sipTest) talk(ctx context.Context, call sip.Call, media bridge.Media) (endedBy, rms string) {
	limit := time.Duration(t.seconds) * time.Second
	if t.waitHangup {
		limit = waitHangupMax
	}
	mediaTimeout := time.Duration(t.cfg.Calls.MediaTimeoutSeconds) * time.Second

	toneFrame := make([]int16, frameSamples)
	if t.tone {
		for i := range toneFrame {
			toneFrame[i] = int16(math.Round(toneAmplitude * math.Sin(2*math.Pi*1000*float64(i)/8000)))
		}
	}
	pl := media.Playout()
	in := make([]int16, frameSamples)

	var totSq, secSq float64
	var totN, secN int
	start := time.Now()
	ticker := time.NewTicker(frameEvery)
	defer ticker.Stop()
	nextProgress := start.Add(time.Second)
	end := start.Add(limit)
	for {
		select {
		case <-ctx.Done():
			t.printf("interrupted, hanging up")
			return "local hangup", dbfs(totSq, totN)
		case cause := <-call.Ended():
			return endCause(cause), dbfs(totSq, totN)
		case err := <-media.Err():
			if err != nil {
				t.printf("media socket error: %v", err)
			}
			return "media stopped", dbfs(totSq, totN)
		case m := <-call.MediaUpdates():
			t.printf("media update: remote media now %s", m.Addr)
			media.Update(m)
		case now := <-ticker.C:
			var err error
			if t.tone {
				err = media.WriteFrame(toneFrame)
			} else {
				err = media.WriteSilence()
			}
			if err != nil {
				t.printf("send error: %v", err)
			}
			// Only frames that carry received audio count for the level.
			if pl != nil && pl.Depth() >= frameEvery {
				pl.Read(in)
				for _, s := range in {
					secSq += float64(s) * float64(s)
				}
				secN += len(in)
			}
			if !now.Before(nextProgress) {
				s := media.Stats()
				t.printf("t=%ds sent=%d recv=%d rms=%s dBFS", int(now.Sub(start).Round(time.Second)/time.Second), s.Sent, s.Received, dbfs(secSq, secN))
				totSq += secSq
				totN += secN
				secSq, secN = 0, 0
				nextProgress = nextProgress.Add(time.Second)
			}
			if t.waitHangup && mediaTimeout > 0 {
				last := media.Stats().LastRx
				if last.IsZero() {
					last = start
				}
				if now.Sub(last) >= mediaTimeout {
					t.printf("no RTP for %s", mediaTimeout)
					return "media stopped", dbfs(totSq+secSq, totN+secN)
				}
			}
			if !now.Before(end) {
				if t.waitHangup {
					return "wait limit reached", dbfs(totSq+secSq, totN+secN)
				}
				return "local hangup", dbfs(totSq+secSq, totN+secN)
			}
		}
	}
}

func endCause(c sip.EndCause) string {
	switch c {
	case sip.RemoteBye:
		return "remote BYE"
	case sip.TransportLost:
		return "transport lost"
	}
	return string(c)
}

func (t *sipTest) finalReport(r report, hadMedia bool) {
	t.printf("")
	t.printf("report:")
	t.printf("  answered:        %s", map[bool]string{true: "yes", false: "no"}[r.answered])
	t.printf("  final status:    %s", r.status)
	if hadMedia {
		t.printf("  packets:         sent=%d received=%d lost=%d late=%d", r.stats.sent, r.stats.recv, r.stats.lost, r.stats.late)
		t.printf("  max gap:         %s", r.stats.maxGap.Round(time.Millisecond))
		latched := r.stats.latched
		if latched == "" {
			latched = "none"
		}
		t.printf("  latched source:  %s", latched)
		t.printf("  received level:  %s dBFS", r.rmsDB)
		t.printf("  ended by:        %s", r.endedBy)
	}
}
