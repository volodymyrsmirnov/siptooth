package cli

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip/siptest"
)

const sipTestPassword = "s3cr3t-pa55-w0rd"

func sipTestConfig(t *testing.T, tr *siptest.Trunk, extra string) string {
	t.Helper()
	return sipTestConfigT(t, tr, "udp", extra)
}

func sipTestConfigT(t *testing.T, tr *siptest.Trunk, transport, extra string) string {
	t.Helper()
	body := fmt.Sprintf("bluetooth: {phone_address: 'AA:BB:CC:DD:EE:FF'}\nsip: {target: '%s', transport: %s%s}\nnat: {rtp_port_min: 41000, rtp_port_max: 42000}\n",
		tr.URI("200"), transport, extra)
	return writeTempConfig(t, body)
}

func answerWithEcho(tr *siptest.Trunk, echo uint16, after func(d *siptest.Dialog)) {
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		body := strings.Join([]string{
			"v=0", "o=pbx 1 1 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
			fmt.Sprintf("m=audio %d RTP/AVP 0", echo), "a=rtpmap:0 PCMU/8000", "",
		}, "\r\n")
		d, err := tr.Accept(req, tx, []byte(body))
		if err == nil && after != nil {
			go after(d)
		}
	})
}

// syncBuffer tolerates log lines the SIP stack writes after the command returned.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func runSIPTest(args ...string) (code int, stdout, stderr string) {
	var o, e syncBuffer
	code = Run(append([]string{"sip", "test"}, args...), &o, &e)
	return code, o.String(), e.String()
}

func TestSIPTestSuccess(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	answerWithEcho(tr, siptest.RTPEcho(t).Port(), nil)
	cfg := sipTestConfig(t, tr, "")

	code, out, errOut := runSIPTest("--config", cfg, "--seconds", "2", "--tone")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"nat: mode=local", "contact: sip:siptooth@127.0.0.1:", "<- 200 OK", "codec: PCMU", "srtp: off",
		"t=1s sent=", "answered:        yes", "latched source:  127.0.0.1:", "ended by:        local hangup"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	m := regexp.MustCompile(`received=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no received= in\n%s", out)
	}
	if n, _ := strconv.Atoi(m[1]); n == 0 {
		t.Fatalf("received 0\n%s", out)
	}
	lm := regexp.MustCompile(`received level:  (\S+) dBFS`).FindStringSubmatch(out)
	if lm == nil {
		t.Fatalf("no received level in\n%s", out)
	}
	if db, err := strconv.ParseFloat(lm[1], 64); err != nil || db < -30 || db > -17 {
		t.Errorf("received level %q, want a -20 dBFS tone within -30..-17\n%s", lm[1], out)
	}
	if !strings.Contains(out, "(source: local)") {
		t.Errorf("contact source missing\n%s", out)
	}
	if len(tr.RequestsFor("BYE")) != 1 {
		t.Errorf("BYE requests = %d, want 1", len(tr.RequestsFor("BYE")))
	}
}

func TestSIPTestRejected(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		tx.Respond(gosip.NewResponseFromRequest(req, 486, "Busy Here", nil))
	})
	code, out, _ := runSIPTest("--config", sipTestConfig(t, tr, ""), "--seconds", "2")
	if code != ExitError {
		t.Fatalf("exit %d, want %d\n%s", code, ExitError, out)
	}
	for _, want := range []string{"<- 486 Busy Here", "answered:        no", "final status:    486 Busy Here"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
}

func TestSIPTestRemoteHangup(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	answerWithEcho(tr, siptest.RTPEcho(t).Port(), func(d *siptest.Dialog) {
		time.Sleep(time.Second)
		_ = d.Bye(t.Context())
	})
	start := time.Now()
	code, out, errOut := runSIPTest("--config", sipTestConfig(t, tr, ""), "--wait-remote-hangup")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "ended by:        remote BYE") {
		t.Errorf("no remote BYE in report\n%s", out)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	if len(tr.RequestsFor("BYE")) != 0 {
		t.Errorf("the test sent its own BYE after the far end ended the call")
	}
}

func TestSIPTestRedactsSecrets(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	echo := siptest.RTPEcho(t).Port()
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if !siptest.CheckDigest(req, "alice", sipTestPassword, "nonce1") {
			siptest.Challenge(req, tx, false, "test", "nonce1", false)
			return
		}
		body := strings.Join([]string{
			"v=0", "o=pbx 1 1 IN IP4 127.0.0.1", "s=-", "c=IN IP4 127.0.0.1", "t=0 0",
			fmt.Sprintf("m=audio %d RTP/AVP 0", echo), "a=rtpmap:0 PCMU/8000", "",
		}, "\r\n")
		tr.Accept(req, tx, []byte(body))
	})
	t.Setenv("SIPTOOTH_SIP_PASSWORD", sipTestPassword)
	t.Setenv("SIPTOOTH_LOG_LEVEL", "debug")
	code, out, errOut := runSIPTest("--config", sipTestConfig(t, tr, ", username: alice"), "--seconds", "1")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "<- 401") {
		t.Errorf("expected the challenge to be listed\n%s", out)
	}
	if !strings.Contains(errOut, "INVITE") {
		t.Errorf("debug log should show the messages\n%s", errOut)
	}
	if strings.Contains(out+errOut, sipTestPassword) {
		t.Fatalf("password leaked\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if m := regexp.MustCompile(`response="[0-9a-f]{32}"`).FindString(out + errOut); m != "" {
		t.Fatalf("digest response leaked: %s", m)
	}
}

func TestSIPTestAlongsideDaemonPort(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	answerWithEcho(tr, siptest.RTPEcho(t).Port(), nil)
	// Something (the daemon) already owns the configured SIP port.
	hold, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	port := hold.LocalAddr().(*net.UDPAddr).Port

	code, out, errOut := runSIPTest("--config", sipTestConfig(t, tr, fmt.Sprintf(", local_port: %d", port)), "--seconds", "1")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, fmt.Sprintf(":%d;", port)) {
		t.Errorf("contact uses the daemon's port %d\n%s", port, out)
	}
}

func TestSIPTestUsage(t *testing.T) {
	if code, _, _ := runSIPTest("--seconds", "0"); code != ExitUsage {
		t.Errorf("--seconds 0: exit %d, want %d", code, ExitUsage)
	}
	var o, e bytes.Buffer
	if code := Run([]string{"sip"}, &o, &e); code != ExitUsage {
		t.Errorf("bare sip: exit %d", code)
	}
}

func TestSIPTestRegistersOwnContact(t *testing.T) {
	tr := siptest.NewTrunk(t, "udp")
	answerWithEcho(tr, siptest.RTPEcho(t).Port(), nil)
	tr.OnRegister(func(req *gosip.Request, tx gosip.ServerTransaction) {
		res := gosip.NewResponseFromRequest(req, 200, "OK", nil)
		for _, h := range req.GetHeaders("Contact") {
			if c, ok := h.(*gosip.ContactHeader); ok {
				cc := gosip.HeaderClone(c).(*gosip.ContactHeader)
				if _, has := cc.Params.Get("expires"); !has {
					cc.Params.Add("expires", "120")
				}
				res.AppendHeader(cc)
			}
		}
		tx.Respond(res)
	})
	cfg := sipTestConfig(t, tr, ", register: true, username: alice")
	t.Setenv("SIPTOOTH_SIP_PASSWORD", sipTestPassword)
	code, out, errOut := runSIPTest("--config", cfg, "--seconds", "1")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"register: ok, contact ", "unregistered"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
	regs := tr.RequestsFor("REGISTER")
	if len(regs) < 2 {
		t.Fatalf("REGISTER requests = %d, want register and unregister", len(regs))
	}
	if last := regs[len(regs)-1]; !strings.Contains(last.Header("Contact"), "expires=0") || strings.Contains(last.Header("Contact"), "*") {
		t.Errorf("last REGISTER Contact = %q, want this UA's contact with expires=0", last.Header("Contact"))
	}
}

func TestSIPTestOptionsProbe(t *testing.T) {
	t.Run("answering", func(t *testing.T) {
		tr := siptest.NewTrunk(t, "udp")
		answerWithEcho(tr, siptest.RTPEcho(t).Port(), nil)
		tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {
			tx.Respond(gosip.NewResponseFromRequest(req, 200, "OK", nil))
		})
		code, out, errOut := runSIPTest("--config", sipTestConfig(t, tr, ""), "--seconds", "1")
		if code != ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		if !regexp.MustCompile(`options: received=127\.0\.0\.1 rport=\d+`).MatchString(out) {
			t.Errorf("no options observation\n%s", out)
		}
	})
	t.Run("silent", func(t *testing.T) {
		tr := siptest.NewTrunk(t, "udp")
		answerWithEcho(tr, siptest.RTPEcho(t).Port(), nil)
		tr.OnOptions(func(req *gosip.Request, tx gosip.ServerTransaction) {}) // never answers
		code, out, errOut := runSIPTest("--config", sipTestConfig(t, tr, ""), "--seconds", "1")
		if code != ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "options: no answer") {
			t.Errorf("no options failure line\n%s", out)
		}
	})
}

// With SRTP the offer carries an inline key; neither the password nor the
// key may reach stdout or the debug log.
func TestSIPTestSRTPKeyRedacted(t *testing.T) {
	tr := siptest.NewTrunk(t, "tls")
	sipTestTLS = tr.ClientTLSConfig()
	t.Cleanup(func() { sipTestTLS = nil })
	echo := siptest.RTPEcho(t)
	keyRe := regexp.MustCompile(`inline:([A-Za-z0-9+/=]{20,})`)
	var offered atomic.Value
	tr.OnInvite(func(req *gosip.Request, tx gosip.ServerTransaction) {
		if !siptest.CheckDigest(req, "alice", sipTestPassword, "nonce1") {
			siptest.Challenge(req, tx, false, "test", "nonce1", false)
			return
		}
		m := keyRe.FindSubmatch(req.Body())
		if m == nil {
			tx.Respond(gosip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
			return
		}
		offered.Store(string(m[1]))
		key, err := base64.StdEncoding.DecodeString(string(m[1]))
		if err != nil {
			t.Error(err)
			return
		}
		// Echo the offered key back as ours: the RTP echo returns our own
		// SRTP packets, which then decrypt with it.
		body := sdp.Offer{SessID: 1, Version: 1, Addr: echo, Codecs: []g711.Codec{g711.PCMU}, LocalKey: key}.Marshal()
		tr.Accept(req, tx, body)
	})
	t.Setenv("SIPTOOTH_SIP_PASSWORD", sipTestPassword)
	t.Setenv("SIPTOOTH_LOG_LEVEL", "debug")
	cfg := sipTestConfigT(t, tr, "tls", ", username: alice, srtp: true")
	code, out, errOut := runSIPTest("--config", cfg, "--seconds", "1", "--tone")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	key, _ := offered.Load().(string)
	if key == "" {
		t.Fatal("the trunk never saw an SRTP offer")
	}
	if !strings.Contains(out, "srtp: on") {
		t.Errorf("srtp not negotiated\n%s", out)
	}
	all := out + errOut
	if !strings.Contains(errOut, "a=crypto") {
		t.Errorf("debug log should show the crypto line (redacted)\n%s", errOut)
	}
	if strings.Contains(all, key) || strings.Contains(all, sipTestPassword) {
		t.Fatalf("secret leaked\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if m := keyRe.FindString(all); m != "" {
		t.Fatalf("inline key material leaked: %s", m)
	}
}
