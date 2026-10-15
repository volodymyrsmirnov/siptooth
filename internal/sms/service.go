package sms

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

// Timings and limits of the service lifecycle (spec 17.7). Package variables so tests can
// shorten the real-time ones; the fake clock drives every timer.
var (
	settleDelay        = 3 * time.Second
	mnsWindow          = 30 * time.Second
	retryDelays        = []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second}
	getRetryDelay      = 10 * time.Second
	getMaxAttempts     = 3
	unavailableAfter   = 5 * time.Minute
	stuckInterval      = 10 * time.Minute
	stuckAfterTimeouts = 2
	shutdownWait       = 2 * time.Second
)

const (
	dedupSize = 1024
	queueMax  = 32
	inboxSize = 64

	permissionHint = "allow message access for siptooth on the phone: Settings → Connected devices → siptooth → Text messages (or answer the phone's message-access prompt)"
)

// Service states (spec 17.7).
const (
	StateDisabled   = "disabled"
	StateWaiting    = "waiting"
	StateConnecting = "connecting"
	StateReady      = "ready"
	StateRetrying   = "retrying"
)

var errNoNotificationChannel = errors.New("phone did not open the notification channel")

// Message is a fetched incoming SMS handed to the Sink.
type Message struct {
	ID, Handle, Type, From, FromName, Text string
}

// Sink receives what the service produces. Both methods are called from the service's loop and
// must not block.
type Sink interface {
	// Deliver reports whether the hook dispatcher accepted the message.
	Deliver(Message) bool
	// Unavailable reports that MAP has not been ready for a long while (system.error).
	Unavailable(detail string)
}

// Dialer is the per-Bluetooth-session way to reach the phone's MAS (btsdp on Linux, fakes in tests).
type Dialer interface {
	FindMAS(ctx context.Context) (btsdp.MASRecord, error)
	Dial(ctx context.Context, channel uint8) (io.ReadWriteCloser, error)
}

// Status is the service snapshot for GET /status (spec 17.10). Disabled is exactly
// {"enabled":false,"state":"disabled"}.
type Status struct {
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	Notifications *bool  `json:"notifications,omitempty"`
	Received      *int   `json:"received,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// Deps are the service's collaborators.
type Deps struct {
	Enabled bool
	Sink    Sink
	Clock   Clock // nil: the real clock
	Logger  *slog.Logger
	NewID   func(time.Time) string // nil: UTC timestamp and 4 random hex digits
	// MNSStuck is called from the loop after two consecutive notification-channel timeouts, at
	// most once per 10 minutes (spec 17.7, "MNS listener recovery"); may be nil; must not block.
	MNSStuck func()
}

// Service owns the MAP state: one loop goroutine owns every state variable and timer, and
// blocking work runs on workers whose results carry the generation they belong to.
type Service struct {
	d     Deps
	log   *slog.Logger
	clock Clock

	inbox  chan any
	stopCh chan struct{} // closed when shutdown begins

	mu      sync.Mutex // guards stopped and mnsRWC
	stopped bool
	mnsRWC  io.Closer
	mnsSeq  uint64 // admission counter, guarded by mu

	regMu  sync.Mutex
	regErr error

	status atomic.Pointer[Status]
	wg     sync.WaitGroup

	// Loop-owned state.
	state      string
	phoneUp    bool
	dialer     Dialer
	gen        uint64 // bumped whenever the attempt/session is torn down
	attempting bool
	attCancel  context.CancelFunc

	mas               *MAS
	rec               btsdp.MASRecord
	masCancel         context.CancelFunc
	masCtx            context.Context
	ops               chan any
	opBusy            bool
	masDead           bool // the MAS connection closed while an operation was in flight
	regPending, regOK bool

	mnsGen    uint64
	mnsActive bool   // a served MNS connection of generation mnsGen exists
	mnsOpen   bool   // its OBEX Connect succeeded
	mnsCurSeq uint64 // admission sequence of the served MNS connection

	queue     []string
	inFlight  string
	retrying  map[string]bool
	tries     map[string]int
	seen      map[string]struct{}
	seenOrder []string

	attemptT, windowT, unavailT slot
	backoff                     int
	mnsTimeouts                 int
	lastStuck                   time.Time
	unavailSent                 bool
	lastError                   string
	received                    int
}

// NewService builds the service. Run starts it.
func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.NewID == nil {
		d.NewID = defaultID
	}
	s := &Service{
		d: d, log: d.Logger.With("component", "sms"), clock: d.Clock,
		inbox: make(chan any, inboxSize), stopCh: make(chan struct{}),
		state: StateWaiting,
	}
	s.resetSession()
	s.publish()
	return s
}

func defaultID(t time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// ---- inputs ----

type (
	linkMsg struct {
		up bool
		d  Dialer
	}
	mnsConnMsg struct {
		rwc io.ReadWriteCloser
		seq uint64
	}
	mnsDiscMsg      struct{}
	mnsConnectedMsg struct{ gen uint64 }
	mnsEventsMsg    struct {
		gen      uint64
		instance int
		evs      []Event
		err      error
	}
	mnsEndedMsg struct {
		gen uint64
		err error
	}
	attemptRes struct {
		gen   uint64
		mas   *MAS
		rec   btsdp.MASRecord
		phase string
		err   error
	}
	regDone struct {
		gen uint64
		err error
	}
	getDone struct {
		gen    uint64
		handle string
		bm     BMessage
		err    error
	}
	masGone  struct{ gen uint64 }
	retryDue struct {
		gen    uint64
		handle string
	}
	opReg struct{}
	opGet struct{ handle string }
)

// post hands a message to the loop from outside it without ever dropping it, unless the
// service is stopping (false then).
func (s *Service) post(m any) bool {
	select {
	case <-s.stopCh:
		return false
	default:
	}
	select {
	case s.inbox <- m:
		return true
	case <-s.stopCh:
		return false
	}
}

// PhoneLink reports that the HFP link came up (with this link's dialer) or went away. It
// never blocks; an up while already up replaces the previous link.
func (s *Service) PhoneLink(up bool, d Dialer) {
	if !s.d.Enabled {
		return
	}
	select {
	case s.inbox <- linkMsg{up: up, d: d}:
	default:
		s.log.Warn("phone link notification dropped", "up", up)
	}
}

// MNSConnection hands over an accepted MNS connection. Admission is non-blocking: when the
// service is disabled or stopped, or its inbox is full, rwc is closed at once. A previous
// MNS connection is replaced and closed.
func (s *Service) MNSConnection(rwc io.ReadWriteCloser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.d.Enabled || s.stopped {
		_ = rwc.Close()
		return
	}
	seq := s.mnsSeq + 1
	select {
	case s.inbox <- mnsConnMsg{rwc: rwc, seq: seq}:
	default:
		s.log.Warn("MNS connection refused: service busy")
		_ = rwc.Close()
		return
	}
	if old := s.mnsRWC; old != nil {
		_ = old.Close()
	}
	s.mnsSeq, s.mnsRWC = seq, rwc
}

// MNSDisconnected closes the current MNS connection before returning (BlueZ's Profile1
// contract), then tells the loop.
func (s *Service) MNSDisconnected() {
	s.mu.Lock()
	rwc := s.mnsRWC
	s.mnsRWC = nil
	if rwc != nil {
		_ = rwc.Close()
	}
	s.mu.Unlock()
	if !s.d.Enabled {
		return
	}
	select {
	case s.inbox <- mnsDiscMsg{}:
	default:
		s.log.Warn("MNS disconnect notification dropped")
	}
}

// SetRegistrationError records the MNS profile registration state for status (nil clears).
func (s *Service) SetRegistrationError(err error) {
	s.regMu.Lock()
	s.regErr = err
	s.regMu.Unlock()
}

// Status returns the latest snapshot.
func (s *Service) Status() Status {
	if !s.d.Enabled {
		return Status{Enabled: false, State: StateDisabled}
	}
	st := *s.status.Load()
	// A failed MNS profile registration is the root cause of any service error.
	s.regMu.Lock()
	if s.regErr != nil {
		st.LastError = "MNS profile registration: " + s.regErr.Error()
	}
	s.regMu.Unlock()
	return st
}

func (s *Service) publish() {
	if !s.d.Enabled {
		return
	}
	n, r := s.mnsOpen, s.received
	s.status.Store(&Status{Enabled: true, State: s.state, Notifications: &n, Received: &r, LastError: s.lastError})
}

func (s *Service) setState(st string) {
	if st != s.state {
		s.state = st
		s.log.Info("sms state", "state", st)
	}
	if st == StateReady {
		s.unavailT.stop()
		s.unavailSent = false
	} else if s.phoneUp && !s.unavailSent && !s.unavailT.armed() {
		s.unavailT.set(s.clock, unavailableAfter)
	}
	s.publish()
}

// ---- loop ----

// Run drives the service until ctx ends, then closes everything and waits up to 2 s for the
// workers.
func (s *Service) Run(ctx context.Context) {
	if !s.d.Enabled {
		<-ctx.Done()
		s.shutdown()
		return
	}
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case m := <-s.inbox:
			s.handle(m)
		case <-s.attemptT.C():
			s.attemptT.t = nil
			s.startAttempt()
		case <-s.windowT.C():
			s.windowT.t = nil
			s.windowExpired()
		case <-s.unavailT.C():
			s.unavailT.t = nil
			if s.phoneUp && s.state != StateReady {
				s.unavailSent = true
				detail := s.lastError
				if detail == "" {
					detail = "message access not ready"
				}
				s.log.Warn("sms unavailable", "detail", detail)
				if s.d.Sink != nil {
					s.d.Sink.Unavailable(detail)
				}
			}
		}
	}
}

func (s *Service) handle(m any) {
	switch m := m.(type) {
	case linkMsg:
		s.teardown()
		if m.up {
			s.phoneUp, s.dialer, s.backoff = true, m.d, 0
			s.attemptT.set(s.clock, settleDelay)
			s.setState(StateConnecting)
		} else {
			s.phoneUp, s.dialer = false, nil
			s.unavailT.stop()
			s.setState(StateWaiting)
		}
	case attemptRes:
		s.onAttempt(m)
	case regDone:
		s.onRegDone(m)
	case getDone:
		s.onGetDone(m)
	case retryDue:
		if m.gen == s.gen && s.mas != nil && s.retrying[m.handle] {
			delete(s.retrying, m.handle)
			s.queue = append([]string{m.handle}, s.queue...)
			s.dispatch()
		}
	case masGone:
		if m.gen != s.gen || s.mas == nil {
			break
		}
		if s.opBusy {
			// The request in flight fails with the real cause now that the transport is
			// gone; its result ends the session (dispatch covers a result that succeeded).
			s.masDead = true
			break
		}
		s.fail(errors.New("MAS connection closed"))
	case mnsConnMsg:
		s.onMNSConn(m)
	case mnsConnectedMsg:
		if m.gen == s.mnsGen && s.mnsActive {
			s.mnsOpen = true
			s.log.Info("MNS connected")
			s.evalReady()
			s.publish()
		}
	case mnsEventsMsg:
		if m.gen == s.mnsGen && s.mnsActive {
			s.onEvents(m)
		}
	case mnsEndedMsg:
		if m.gen == s.mnsGen && s.mnsActive {
			s.log.Info("MNS connection ended", "err", m.err)
			s.mnsGone()
		}
	case mnsDiscMsg:
		if s.mnsActive {
			s.log.Info("MNS disconnect requested")
			s.mnsGone()
		}
	}
}

// resetSession clears the per-MAS-session state.
func (s *Service) resetSession() {
	s.queue, s.inFlight = nil, ""
	s.retrying = map[string]bool{}
	s.tries = map[string]int{}
	s.seen = map[string]struct{}{}
	s.seenOrder = nil
	s.opBusy, s.regPending, s.regOK, s.masDead = false, false, false, false
}

// closeMNS closes the current MNS connection (under the admission mutex) and forgets it.
func (s *Service) closeMNS() {
	s.mu.Lock()
	if s.mnsRWC != nil {
		_ = s.mnsRWC.Close()
		s.mnsRWC = nil
	}
	s.mu.Unlock()
	s.mnsActive, s.mnsOpen = false, false
	s.mnsGen++
}

// teardown ends the attempt and the MAS session (spec 17.7 step 7) and invalidates every
// outstanding worker result.
func (s *Service) teardown() {
	s.gen++
	s.attemptT.stop()
	s.windowT.stop()
	if s.attCancel != nil {
		s.attCancel()
		s.attCancel = nil
	}
	s.attempting = false
	if s.mas != nil {
		_ = s.mas.Close()
		s.mas = nil
	}
	if s.masCancel != nil {
		s.masCancel()
		s.masCancel = nil
	}
	s.ops = nil
	lost := append([]string(nil), s.queue...)
	if s.inFlight != "" {
		lost = append(lost, s.inFlight)
	}
	for h := range s.retrying {
		lost = append(lost, h)
	}
	for _, h := range lost {
		s.log.Warn("message lost with the MAP session", "handle", h)
	}
	s.closeMNS()
	s.resetSession()
}

// fail ends the session after an error and schedules the next attempt.
func (s *Service) fail(err error) {
	s.lastError = err.Error()
	s.teardown()
	if !s.phoneUp {
		s.setState(StateWaiting)
		return
	}
	d := retryDelays[min(s.backoff, len(retryDelays)-1)]
	s.backoff++
	s.attemptT.set(s.clock, d)
	s.log.Warn("MAP session failed", "err", err, "retry_in", d.String())
	s.setState(StateRetrying)
}

// ---- attempt ----

func (s *Service) startAttempt() {
	ctx, cancel := context.WithCancel(context.Background())
	s.attCancel, s.attempting = cancel, true
	s.setState(StateConnecting)
	s.wg.Add(1)
	go s.runAttempt(ctx, s.gen, s.dialer)
}

func (s *Service) runAttempt(ctx context.Context, gen uint64, d Dialer) {
	defer s.wg.Done()
	res := attemptRes{gen: gen, phase: "sdp lookup"}
	rec, err := d.FindMAS(ctx)
	if err == nil {
		res.rec, res.phase = rec, "rfcomm dial"
		var rwc io.ReadWriteCloser
		if rwc, err = d.Dial(ctx, rec.Channel); err == nil {
			if ctx.Err() != nil {
				_ = rwc.Close()
				err = ctx.Err()
			} else {
				res.phase = "obex connect"
				res.mas, err = ConnectMAS(ctx, rwc, rec)
			}
		}
	}
	res.err = err
	if !s.post(res) && res.mas != nil {
		_ = res.mas.Close()
	}
}

func (s *Service) onAttempt(m attemptRes) {
	if m.gen != s.gen || !s.attempting {
		if m.mas != nil {
			_ = m.mas.Close()
		}
		return
	}
	s.attempting = false
	if s.attCancel != nil {
		s.attCancel()
		s.attCancel = nil
	}
	if m.err != nil {
		var re *obex.ResponseError
		if m.phase == "obex connect" && (errors.Is(m.err, io.EOF) || errors.Is(m.err, io.ErrUnexpectedEOF) ||
			(errors.As(m.err, &re) && (re.Code == obex.RespUnauthorized || re.Code == obex.RespForbidden))) {
			s.log.Warn(permissionHint)
		}
		s.fail(m.err)
		return
	}
	// Hand-over: the loop owns the MAS connection from here.
	s.closeMNS() // a stale notification channel belongs to an older MAP session
	ctx, cancel := context.WithCancel(context.Background())
	s.mas, s.rec, s.masCancel, s.masCtx = m.mas, m.rec, cancel, ctx
	s.ops = make(chan any, 1)
	s.regPending = true
	s.log.Info("MAS connected", "channel", m.rec.Channel, "instance", m.rec.InstanceID)
	s.wg.Add(2)
	go s.opWorker(ctx, s.gen, s.mas, s.ops)
	go s.watchMAS(ctx, s.gen, s.mas)
	s.dispatch()
}

func (s *Service) watchMAS(ctx context.Context, gen uint64, m *MAS) {
	defer s.wg.Done()
	select {
	case <-m.Done():
		s.post(masGone{gen: gen})
	case <-ctx.Done():
	}
}

// opWorker is the only goroutine issuing requests on the MAS connection.
func (s *Service) opWorker(ctx context.Context, gen uint64, m *MAS, ops <-chan any) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-ops:
			switch op := op.(type) {
			case opReg:
				s.post(regDone{gen: gen, err: m.RegisterNotifications()})
			case opGet:
				bm, err := m.GetMessage(op.handle)
				s.post(getDone{gen: gen, handle: op.handle, bm: bm, err: err})
			}
		}
	}
}

// dispatch gives the idle operation worker its next job: registration first, then handles.
func (s *Service) dispatch() {
	if s.mas == nil || s.opBusy {
		return
	}
	if s.masDead {
		s.fail(errors.New("MAS connection closed"))
		return
	}
	if s.regPending {
		s.regPending, s.opBusy = false, true
		s.ops <- opReg{}
		return
	}
	if len(s.queue) > 0 {
		h := s.queue[0]
		s.queue = s.queue[1:]
		s.inFlight, s.opBusy = h, true
		s.ops <- opGet{handle: h}
	}
}

func (s *Service) onRegDone(m regDone) {
	if m.gen != s.gen || s.mas == nil {
		return
	}
	s.opBusy = false
	if m.err != nil {
		s.fail(m.err)
		return
	}
	if s.regPending { // asked again meanwhile (MNS lost during the request)
		s.dispatch()
		return
	}
	s.regOK = true
	s.log.Info("notifications registered")
	if s.mnsOpen {
		s.evalReady()
	} else {
		s.windowT.set(s.clock, mnsWindow)
	}
	s.dispatch()
}

// evalReady enters ready once MAS, registration and the MNS connection are all up.
func (s *Service) evalReady() {
	if s.mas == nil || !s.regOK || !s.mnsOpen || s.state == StateReady {
		return
	}
	s.windowT.stop()
	s.backoff, s.mnsTimeouts = 0, 0
	s.lastError = ""
	s.setState(StateReady)
}

func (s *Service) windowExpired() {
	s.mnsTimeouts++
	if s.mnsTimeouts >= stuckAfterTimeouts && s.d.MNSStuck != nil {
		now := s.clock.Now()
		if s.lastStuck.IsZero() || now.Sub(s.lastStuck) >= stuckInterval {
			s.lastStuck, s.mnsTimeouts = now, 0
			s.log.Warn("MNS listener looks stuck, asking for re-registration")
			s.d.MNSStuck()
		}
	}
	s.fail(errNoNotificationChannel)
}

// ---- MNS ----

func (s *Service) onMNSConn(m mnsConnMsg) {
	if s.mnsOpen { // replaced; the admission already closed the old connection
		s.mnsGone()
	}
	s.mnsGen++
	gen := s.mnsGen
	s.mnsActive, s.mnsOpen, s.mnsCurSeq = true, false, m.seq
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := ServeMNS(m.rwc,
			func() { s.post(mnsConnectedMsg{gen: gen}) },
			func(instance int, evs []Event, err error) {
				s.post(mnsEventsMsg{gen: gen, instance: instance, evs: evs, err: err})
			})
		s.post(mnsEndedMsg{gen: gen, err: err})
	}()
	s.publish()
}

// mnsGone handles the loss of the served MNS connection (spec 17.7 step 6): with a MAS session
// up, notifications are off, so registration is queued again.
func (s *Service) mnsGone() {
	wasOpen := s.mnsOpen
	s.mnsActive, s.mnsOpen = false, false
	s.mu.Lock()
	if s.mnsRWC != nil && s.mnsSeq == s.mnsCurSeq {
		_ = s.mnsRWC.Close()
		s.mnsRWC = nil
	}
	s.mu.Unlock()
	if s.mas != nil && wasOpen {
		s.regOK, s.regPending = false, true
		s.windowT.stop()
		s.setState(StateConnecting)
		s.dispatch()
	}
	s.publish()
}

func (s *Service) onEvents(m mnsEventsMsg) {
	if m.err != nil {
		s.log.Warn("MNS event report unreadable", "err", m.err)
		return
	}
	for _, ev := range m.evs {
		if !ev.Forward() {
			if ev.IsMMS() {
				s.log.Info("MMS notification ignored (not forwarded)", "type", ev.Type, "msg_type", ev.MsgType, "folder", ev.Folder, "handle", ev.Handle)
			} else {
				s.log.Debug("event ignored", "type", ev.Type, "msg_type", ev.MsgType, "folder", ev.Folder, "handle", ev.Handle)
			}
			continue
		}
		if s.mas == nil {
			s.log.Warn("message event dropped: no MAP session", "handle", ev.Handle)
			continue
		}
		if m.instance >= 0 && m.instance != int(s.rec.InstanceID) {
			s.log.Debug("event for another MAS instance ignored", "instance", m.instance, "handle", ev.Handle)
			continue
		}
		s.enqueue(ev.Handle)
	}
}

// enqueue queues a handle unless it is in the session's dedup window; at most 32 wait (the
// oldest is dropped).
func (s *Service) enqueue(h string) {
	if h == "" {
		return
	}
	if _, dup := s.seen[h]; dup {
		s.log.Debug("duplicate event ignored", "handle", h)
		return
	}
	s.seen[h] = struct{}{}
	s.seenOrder = append(s.seenOrder, h)
	if len(s.seenOrder) > dedupSize {
		delete(s.seen, s.seenOrder[0])
		s.seenOrder = s.seenOrder[1:]
	}
	if len(s.queue) >= queueMax {
		s.log.Warn("fetch queue full, oldest message dropped", "handle", s.queue[0])
		s.queue = s.queue[1:]
	}
	s.queue = append(s.queue, h)
	s.dispatch()
}

// ---- fetch results ----

func (s *Service) onGetDone(m getDone) {
	if m.gen != s.gen || s.mas == nil {
		s.log.Warn("message result discarded: session ended", "handle", m.handle)
		return
	}
	s.opBusy, s.inFlight = false, ""
	switch ClassifyGetError(m.err) {
	case GetDeliver:
		delete(s.tries, m.handle)
		s.deliver(m)
	case GetDrop:
		delete(s.tries, m.handle)
		s.log.Warn("message dropped", "handle", m.handle, "err", m.err)
	case GetRetry:
		s.tries[m.handle]++
		if s.tries[m.handle] >= getMaxAttempts {
			delete(s.tries, m.handle)
			s.log.Warn("message dropped after retries", "handle", m.handle, "err", m.err)
			break
		}
		s.log.Warn("message fetch failed, retrying", "handle", m.handle, "err", m.err, "retry_in", getRetryDelay.String())
		s.retrying[m.handle] = true
		s.wg.Add(1)
		go s.retryTimer(s.gen, m.handle, s.masCtx)
	case GetEndSession:
		var re *obex.ResponseError
		if errors.As(m.err, &re) && re.Code == obex.RespUnauthorized {
			s.log.Warn(permissionHint)
		}
		s.log.Warn("message lost: fetch failed", "handle", m.handle)
		s.fail(m.err)
		return
	}
	s.dispatch()
}

func (s *Service) retryTimer(gen uint64, handle string, ctx context.Context) {
	defer s.wg.Done()
	t := s.clock.NewTimer(getRetryDelay)
	defer t.Stop()
	select {
	case <-t.C():
		s.post(retryDue{gen: gen, handle: handle})
	case <-ctx.Done():
	}
}

func (s *Service) deliver(m getDone) {
	if s.d.Sink == nil {
		return
	}
	msg := Message{ID: s.d.NewID(s.clock.Now()), Handle: m.handle, Type: m.bm.Type, From: m.bm.From, FromName: m.bm.FromName, Text: m.bm.Text}
	if s.d.Sink.Deliver(msg) {
		s.received++
		s.publish()
		return
	}
	s.log.Warn("message not accepted by the hook dispatcher", "handle", m.handle)
}

// ---- shutdown ----

func (s *Service) shutdown() {
	s.mu.Lock()
	s.stopped = true
	if s.mnsRWC != nil {
		_ = s.mnsRWC.Close()
		s.mnsRWC = nil
	}
	s.mu.Unlock()
	close(s.stopCh)
	s.phoneUp = false
	s.teardown()
	s.unavailT.stop()
	s.state = StateWaiting
	s.publish()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	t := time.NewTimer(shutdownWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		s.log.Warn("sms workers did not stop in time")
	}
	for {
		select {
		case m := <-s.inbox:
			switch m := m.(type) {
			case mnsConnMsg:
				_ = m.rwc.Close()
			case attemptRes:
				if m.mas != nil {
					_ = m.mas.Close()
				}
			}
		default:
			return
		}
	}
}
