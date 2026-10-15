package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

type fakeEmitter struct {
	sms    []hooks.SMSInfo
	accept bool
	errs   [][2]string
}

func (f *fakeEmitter) EmitSMS(m hooks.SMSInfo) bool { f.sms = append(f.sms, m); return f.accept }
func (f *fakeEmitter) EmitError(code, detail string) {
	f.errs = append(f.errs, [2]string{code, detail})
}

func TestSMSSinkDeliver(t *testing.T) {
	var logs bytes.Buffer
	em := &fakeEmitter{accept: true}
	s := smsSink{hooks: em, log: slog.New(slog.NewTextHandler(&logs, nil))}
	ok := s.Deliver(sms.Message{ID: "i1", Handle: "h", Type: "SMS_GSM", From: "+100", FromName: "Jane Secret", Text: "héllo wörld"})
	if !ok || len(em.sms) != 1 {
		t.Fatalf("ok=%v sms=%v", ok, em.sms)
	}
	got := em.sms[0]
	if got.ID != "i1" || got.From == nil || *got.From != "+100" || got.FromName == nil || *got.FromName != "Jane Secret" || got.Text != "héllo wörld" {
		t.Fatalf("info = %+v", got)
	}
	line := logs.String()
	for _, want := range []string{"msg=\"sms received\"", "sms_id=i1", "from=+100", "type=SMS_GSM", "text_chars=11"} {
		if !strings.Contains(line, want) {
			t.Errorf("log lacks %q: %s", want, line)
		}
	}
	for _, bad := range []string{"héllo", "Jane", "Secret"} {
		if strings.Contains(line, bad) {
			t.Errorf("log leaks %q: %s", bad, line)
		}
	}
}

func TestSMSSinkUnknownSenderAndRejected(t *testing.T) {
	var logs bytes.Buffer
	em := &fakeEmitter{}
	s := smsSink{hooks: em, log: slog.New(slog.NewTextHandler(&logs, nil))}
	if s.Deliver(sms.Message{ID: "i2", Text: "x"}) {
		t.Fatal("must report the dispatcher's refusal")
	}
	if em.sms[0].From != nil || em.sms[0].FromName != nil {
		t.Fatalf("unknown sender must be nil: %+v", em.sms[0])
	}
	if !strings.Contains(logs.String(), "msg=\"sms received\"") {
		t.Fatalf("info line missing: %s", logs.String())
	}
}

func TestSMSSinkUnavailable(t *testing.T) {
	em := &fakeEmitter{}
	smsSink{hooks: em, log: quietLogger()}.Unavailable("no MAS")
	if len(em.errs) != 1 || em.errs[0] != [2]string{"sms_unavailable", "no MAS"} {
		t.Fatalf("errs = %v", em.errs)
	}
}

type fakeLink struct {
	mu      sync.Mutex
	conns   int
	discs   int
	regErrs []error
}

func (f *fakeLink) MNSConnection(io.ReadWriteCloser) { f.mu.Lock(); f.conns++; f.mu.Unlock() }
func (f *fakeLink) MNSDisconnected()                 { f.mu.Lock(); f.discs++; f.mu.Unlock() }
func (f *fakeLink) SetRegistrationError(e error) {
	f.mu.Lock()
	f.regErrs = append(f.regErrs, e)
	f.mu.Unlock()
}

func TestMNSHandlerRejectsOtherDevices(t *testing.T) {
	link := &fakeLink{}
	h := newMNSHandler(link, "hci0", "aa:bb:cc:dd:ee:ff", quietLogger())
	other, _ := os.CreateTemp(t.TempDir(), "fd")
	if err := h.NewConnection("/org/bluez/hci0/dev_11_22_33_44_55_66", other); err == nil {
		t.Fatal("other device accepted")
	}
	if _, err := other.Stat(); err == nil {
		t.Fatal("rejected fd must be closed")
	}
	if link.conns != 0 {
		t.Fatal("rejected connection reached the service")
	}
	phone, _ := os.CreateTemp(t.TempDir(), "fd")
	defer phone.Close()
	if err := h.NewConnection("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF", phone); err != nil || link.conns != 1 {
		t.Fatalf("err=%v conns=%d", err, link.conns)
	}
	h.Disconnected("x")
	if link.discs != 1 {
		t.Fatal("Disconnected not forwarded")
	}
}

type fakePhoneService struct {
	ups   []bool
	dials []sms.Dialer
}

func (f *fakePhoneService) PhoneLink(up bool, d sms.Dialer) {
	f.ups = append(f.ups, up)
	f.dials = append(f.dials, d)
}

func TestPhoneLinkClosure(t *testing.T) {
	f := &fakePhoneService{}
	fn := phoneLink(f, "AA:BB:CC:DD:EE:FF")
	fn(true, "11:22:33:44:55:66")
	fn(false, "")
	if len(f.ups) != 2 || !f.ups[0] || f.ups[1] {
		t.Fatalf("ups = %v", f.ups)
	}
	if d, ok := f.dials[0].(btDialer); !ok || d.adapter != "11:22:33:44:55:66" || d.phone != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("dialer = %#v", f.dials[0])
	}
	if f.dials[1] != nil {
		t.Fatal("down must carry a nil dialer")
	}
}

func TestSMSBindingCoalesces(t *testing.T) {
	b := newSMSBinding(&fakeLink{})
	b.requestReregister()
	b.requestReregister() // must not block
	<-b.stuck
	select {
	case <-b.stuck:
		t.Fatal("requests must coalesce")
	default:
	}
}

// registrarHarness drives an mnsRegistrar like the session loop does.
type registrarHarness struct {
	r     *mnsRegistrar
	link  *fakeLink
	wg    sync.WaitGroup
	calls chan mnsKind
	gate  chan struct{} // closed or fed to let run return
	errs  chan error
}

func newHarness(t *testing.T) *registrarHarness {
	t.Helper()
	h := &registrarHarness{link: &fakeLink{}, calls: make(chan mnsKind, 8), gate: make(chan struct{}, 8), errs: make(chan error, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); h.wg.Wait() })
	run := func(ctx context.Context, k mnsKind) error {
		h.calls <- k
		select {
		case <-h.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-h.errs:
			return err
		default:
			return nil
		}
	}
	h.r = newMNSRegistrar(ctx, &h.wg, run, h.link.SetRegistrationError, quietLogger())
	h.r.retryEvery = 20 * time.Millisecond
	h.r.limit = 100 * time.Millisecond
	return h
}

func (h *registrarHarness) next(t *testing.T) mnsResult {
	t.Helper()
	select {
	case r := <-h.r.Results():
		h.r.Handle(r)
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no result")
	}
	return mnsResult{}
}

func TestRegistrarSuccessClearsError(t *testing.T) {
	h := newHarness(t)
	h.errs <- errors.New("boom")
	h.gate <- struct{}{}
	h.r.Request(mnsRegister)
	h.next(t)
	if h.r.Retry() == nil {
		t.Fatal("failure must arm the retry")
	}
	select {
	case <-h.r.Retry():
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not fire")
	}
	h.gate <- struct{}{}
	h.r.RetryDue()
	h.next(t)
	h.link.mu.Lock()
	defer h.link.mu.Unlock()
	if len(h.link.regErrs) != 2 || h.link.regErrs[0] == nil || h.link.regErrs[1] != nil || h.r.Retry() != nil {
		t.Fatalf("regErrs = %v", h.link.regErrs)
	}
}

func TestRegistrarRetriesFailedUnregisterAsReregister(t *testing.T) {
	h := newHarness(t)
	h.errs <- fmt.Errorf("%w: busy", errUnregister)
	h.gate <- struct{}{}
	h.r.Request(mnsReregister)
	<-h.calls
	h.next(t)
	<-h.r.Retry()
	h.gate <- struct{}{}
	h.r.RetryDue()
	if k := <-h.calls; k != mnsReregister {
		t.Fatalf("retry ran %v, want reregister", k)
	}
	h.next(t)
	// A plain failure (or a failed register step) retries as a register.
	h.errs <- errors.New("register failed")
	h.gate <- struct{}{}
	h.r.Request(mnsReregister)
	<-h.calls
	h.next(t)
	<-h.r.Retry()
	h.gate <- struct{}{}
	h.r.RetryDue()
	if k := <-h.calls; k != mnsRegister {
		t.Fatalf("retry ran %v, want register", k)
	}
	h.next(t)
}

func TestRegistrarMergesRequestsWhileBusy(t *testing.T) {
	h := newHarness(t)
	h.r.Request(mnsRegister)
	if k := <-h.calls; k != mnsRegister {
		t.Fatalf("first = %v", k)
	}
	h.r.Request(mnsRegister)
	h.r.Request(mnsReregister)
	h.r.Request(mnsRegister)
	h.gate <- struct{}{}
	h.next(t)
	if k := <-h.calls; k != mnsReregister {
		t.Fatalf("follow-up = %v", k)
	}
	h.gate <- struct{}{}
	h.next(t)
	select {
	case k := <-h.calls:
		t.Fatalf("unexpected extra run %v", k)
	default:
	}
}

func TestRegistrarStaleResultIgnored(t *testing.T) {
	h := newHarness(t)
	h.errs <- errors.New("old bus")
	h.r.Request(mnsRegister)
	<-h.calls
	h.r.Invalidate()
	h.gate <- struct{}{}
	h.next(t) // stale, nothing pending
	if h.r.Retry() != nil || len(h.link.regErrs) != 0 {
		t.Fatalf("stale result acted on: retry=%v errs=%v", h.r.Retry() != nil, h.link.regErrs)
	}
}

func TestRegistrarWorkerIsBounded(t *testing.T) {
	h := newHarness(t)
	h.r.Request(mnsRegister) // the gate never opens: the 100 ms limit ends the worker
	r := h.next(t)
	if !errors.Is(r.err, context.DeadlineExceeded) || h.r.Retry() == nil {
		t.Fatalf("err=%v retry=%v", r.err, h.r.Retry() != nil)
	}
}
