package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/volodymyrsmirnov/siptooth/internal/bluez"
	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/hooks"
	"github.com/volodymyrsmirnov/siptooth/internal/sms"
)

// smsEmitter is the part of *hooks.Dispatcher the SMS sink uses.
type smsEmitter interface {
	EmitSMS(m hooks.SMSInfo) bool
	EmitError(code, detail string)
}

// smsSink adapts the SMS service's output to the hook dispatcher and the log (spec 17.9, 17.10).
type smsSink struct {
	hooks smsEmitter
	log   *slog.Logger
}

// Deliver queues the sms.received event and logs the one info line per message. The text and the
// sender's name never reach the log.
func (s smsSink) Deliver(m sms.Message) bool {
	ok := s.hooks.EmitSMS(hooks.SMSInfo{
		ID: m.ID, Handle: m.Handle, Type: m.Type, Text: m.Text,
		From: optional(m.From), FromName: optional(m.FromName),
	})
	s.log.Info("sms received", "sms_id", m.ID, "from", m.From, "type", m.Type, "text_chars", utf8.RuneCountInString(m.Text))
	if !ok {
		s.log.Warn("sms not queued for hooks: the hook queue is full", "sms_id", m.ID)
	}
	return ok
}

// Unavailable emits the sms_unavailable system.error (spec 17.7).
func (s smsSink) Unavailable(detail string) { s.hooks.EmitError("sms_unavailable", detail) }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// btDialer reaches the phone's MAS over the given adapter (spec 17.4).
type btDialer struct{ adapter, phone string }

func (d btDialer) FindMAS(ctx context.Context) (btsdp.MASRecord, error) {
	return btsdp.FindSMSMAS(ctx, d.adapter, d.phone)
}

func (d btDialer) Dial(ctx context.Context, channel uint8) (io.ReadWriteCloser, error) {
	f, err := btsdp.DialRFCOMM(ctx, d.adapter, d.phone, channel)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// phoneLink is the coordinator's PhoneLink callback: it never blocks (the service posts to its
// inbox without waiting).
func phoneLink(svc interface{ PhoneLink(bool, sms.Dialer) }, phone string) func(up bool, adapterAddr string) {
	return func(up bool, adapterAddr string) {
		if !up {
			svc.PhoneLink(false, nil)
			return
		}
		svc.PhoneLink(true, btDialer{adapter: adapterAddr, phone: phone})
	}
}

// smsLink is the part of *sms.Service the Bluetooth session uses.
type smsLink interface {
	MNSConnection(rwc io.ReadWriteCloser)
	MNSDisconnected()
	SetRegistrationError(err error)
}

// smsBinding connects the Bluetooth session to the SMS service. A nil binding means SMS is off.
type smsBinding struct {
	link  smsLink
	stuck chan struct{} // MNSStuck requests (capacity 1)
}

func newSMSBinding(link smsLink) *smsBinding {
	return &smsBinding{link: link, stuck: make(chan struct{}, 1)}
}

// requestReregister asks the Bluetooth session to unregister and register the MNS profile again;
// it never blocks and coalesces requests.
func (b *smsBinding) requestReregister() {
	select {
	case b.stuck <- struct{}{}:
	default:
	}
}

var errUnexpectedDevice = errors.New("unexpected device")

// mnsHandler is the bluez.ProfileHandler of the MNS profile (spec 17.8).
type mnsHandler struct {
	link      smsLink
	phonePath string // D-Bus object path of the configured phone
	log       *slog.Logger
}

func newMNSHandler(link smsLink, adapter, phone string, log *slog.Logger) *mnsHandler {
	return &mnsHandler{link: link, phonePath: bluez.DevicePath(adapter, phone), log: log}
}

// NewConnection passes the phone's notification connection to the service; any other device is
// rejected and its descriptor closed, as for HFP.
func (h *mnsHandler) NewConnection(device string, fd *os.File) error {
	if !strings.EqualFold(device, h.phonePath) {
		fd.Close()
		h.log.Warn("bluetooth: rejected MNS connection from unexpected device", "device", device)
		return errUnexpectedDevice
	}
	h.link.MNSConnection(fd)
	return nil
}

// Disconnected closes the live MNS connection synchronously (BlueZ's Profile1 contract).
func (h *mnsHandler) Disconnected(string) { h.link.MNSDisconnected() }

const (
	mnsRegisterLimit = 10 * time.Second // one registration attempt (spec 17.8)
	mnsRetryInterval = 10 * time.Second // while the profile is not registered
)

type mnsKind int

const (
	mnsRegister   mnsKind = iota + 1
	mnsReregister         // unregister, then register
)

// errUnregister marks a reregister that failed at its unregister step; the retry then repeats
// the whole reregister (a plain register would map AlreadyExists to success and leave the
// stale registration in place).
var errUnregister = errors.New("unregister MNS profile")

type mnsResult struct {
	gen  uint64
	kind mnsKind
	err  error
}

// mnsRegistrar runs the MNS profile registration on one worker at a time (spec 17.8). Its
// methods are called from the Bluetooth session loop only; the worker never touches its state.
// Requests that arrive while a worker runs are merged into one follow-up; Invalidate marks the
// running worker's result stale (the bus-session state it belongs to is gone).
type mnsRegistrar struct {
	ctx        context.Context
	run        func(ctx context.Context, kind mnsKind) error
	report     func(error)
	log        *slog.Logger
	limit      time.Duration
	retryEvery time.Duration
	wg         *sync.WaitGroup

	res     chan mnsResult
	retry   <-chan time.Time
	retryAs mnsKind // the kind the next retry runs
	gen     uint64
	busy    bool
	pending mnsKind
}

func newMNSRegistrar(ctx context.Context, wg *sync.WaitGroup, run func(context.Context, mnsKind) error, report func(error), log *slog.Logger) *mnsRegistrar {
	return &mnsRegistrar{ctx: ctx, wg: wg, run: run, report: report, log: log,
		limit: mnsRegisterLimit, retryEvery: mnsRetryInterval, res: make(chan mnsResult, 1)}
}

// Results delivers finished workers; pass each to Handle.
func (r *mnsRegistrar) Results() <-chan mnsResult { return r.res }

// Retry fires when a failed registration is due again; nil when none is waiting. Pass it to
// RetryDue.
func (r *mnsRegistrar) Retry() <-chan time.Time { return r.retry }

// RetryDue starts the retry registration.
func (r *mnsRegistrar) RetryDue() {
	kind := r.retryAs
	if kind == 0 {
		kind = mnsRegister
	}
	r.Request(kind)
}

// Invalidate discards the result of the worker in flight (call before requesting a fresh
// registration after bluetoothd restarted).
func (r *mnsRegistrar) Invalidate() { r.gen++ }

// Request starts a registration, or merges into the one in flight.
func (r *mnsRegistrar) Request(kind mnsKind) {
	r.retry = nil
	if r.busy {
		if kind > r.pending {
			r.pending = kind
		}
		return
	}
	r.start(kind)
}

func (r *mnsRegistrar) start(kind mnsKind) {
	r.busy = true
	gen := r.gen
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		cctx, cancel := context.WithTimeout(r.ctx, r.limit)
		err := r.run(cctx, kind)
		cancel()
		select {
		case r.res <- mnsResult{gen: gen, kind: kind, err: err}:
		case <-r.ctx.Done():
		}
	}()
}

// Handle processes a finished worker.
func (r *mnsRegistrar) Handle(res mnsResult) {
	r.busy = false
	if r.pending != 0 {
		next := r.pending
		r.pending = 0
		r.start(next)
		return
	}
	if res.gen != r.gen {
		return
	}
	if res.err != nil {
		r.log.Warn("bluetooth: MNS profile registration failed, retrying in 10 s", "err", res.err)
		r.report(res.err)
		r.retryAs = mnsRegister
		if res.kind == mnsReregister && errors.Is(res.err, errUnregister) {
			r.retryAs = mnsReregister
		}
		r.retry = time.After(r.retryEvery)
		return
	}
	r.report(nil)
	r.retry = nil
}
