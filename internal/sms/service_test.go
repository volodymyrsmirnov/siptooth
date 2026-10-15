package sms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

func TestReadyOnlyAfterMNSConnect(t *testing.T) {
	r := startedRig(t)
	if r.svc.Status().State != StateWaiting {
		t.Fatalf("initial %+v", r.svc.Status())
	}
	r.svc.PhoneLink(true, r.dialer)
	r.waitState(StateConnecting)
	// The 30 s window starts at registration success, not before.
	r.clock.waitTimer(t, settleDelay)
	if r.clock.has(mnsWindow) {
		t.Fatal("window armed before the MAS session")
	}
	r.clock.Advance(settleDelay)
	mse := r.nextMSE()
	waitCh(t, mse.regs, "registration")
	r.clock.waitTimer(t, mnsWindow)
	if st := r.svc.Status(); st.State != StateConnecting || *st.Notifications {
		t.Fatalf("status %+v", st)
	}
	// No event ever arrives: the MNS OBEX Connect alone makes the service ready.
	r.connectMNS()
	r.waitState(StateReady)
	st := r.svc.Status()
	if !*st.Notifications || *st.Received != 0 || st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	if r.clock.has(mnsWindow) {
		t.Fatal("window still armed when ready")
	}
}

func TestCancelDuringSDP(t *testing.T) {
	r := startedRig(t)
	entered, returned := make(chan struct{}), make(chan error, 1)
	r.dialer.find = func(ctx context.Context) (btsdp.MASRecord, error) {
		close(entered)
		<-ctx.Done()
		returned <- ctx.Err()
		return btsdp.MASRecord{}, ctx.Err()
	}
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	waitClosed(t, entered, "FindMAS")
	r.svc.PhoneLink(false, nil)
	if err := waitCh(t, returned, "FindMAS to see the cancel"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.waitState(StateWaiting)
	if len(r.clock.pending()) != 0 {
		t.Fatalf("timers left: %v", r.clock.pending())
	}
}

func TestCancelDuringDialAndLateConnectionClosed(t *testing.T) {
	r := startedRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	far := make(chan net.Conn, 1)
	// A dialer that ignores ctx and delivers a connection after the attempt was abandoned.
	r.dialer.dial = func(ctx context.Context, ch uint8) (io.ReadWriteCloser, error) {
		close(entered)
		<-release
		a, b := net.Pipe()
		far <- b
		return a, nil
	}
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	waitClosed(t, entered, "Dial")
	r.svc.PhoneLink(false, nil)
	r.waitState(StateWaiting)
	close(release)
	b := waitCh(t, far, "late connection")
	_ = b.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late connection not closed: %v", err)
	}
}

func TestCancelDuringOBEXConnect(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) { m.connectGate = make(chan struct{}) }
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	mse := r.nextMSE()
	waitCh(t, mse.gotConnect, "Connect request")
	r.svc.PhoneLink(false, nil)
	waitClosed(t, mse.done, "transport closed at once (no 40 s wait)")
	r.waitState(StateWaiting)
}

func TestCancelDuringRegistration(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) { m.regGate = make(chan struct{}) }
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	mse := r.nextMSE()
	waitCh(t, mse.regs, "registration request")
	r.svc.PhoneLink(false, nil)
	waitClosed(t, mse.done, "MAS closed")
	r.waitState(StateWaiting)
	if r.clock.has(mnsWindow) {
		t.Fatal("window armed after a cancelled registration")
	}
}

func TestRepeatedUpReplacesSession(t *testing.T) {
	r := startedRig(t)
	mse1, phone1 := r.ready()
	r.svc.PhoneLink(true, r.dialer)
	waitClosed(t, mse1.done, "old MAS closed")
	waitClosed(t, phone1.c.Done(), "old MNS closed")
	r.waitState(StateConnecting)
	mse2 := r.attemptAfter(settleDelay)
	if mse2 == mse1 {
		t.Fatal("same session")
	}
	r.connectMNS()
	r.waitState(StateReady)
}

func TestMNSLossDuringGetReRegistersAfterIt(t *testing.T) {
	r := startedRig(t)
	gate := make(chan struct{})
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(h string) obex.Packet { <-gate; return getOK(h) }
	}
	mse, phone := r.ready()
	phone.event("h1")
	if h := waitCh(t, mse.gets, "get"); h != "h1" {
		t.Fatal(h)
	}
	phone.c.Close() // MNS lost while the Get is in flight
	r.waitState(StateConnecting)
	select {
	case <-mse.regs:
		t.Fatal("registration overtook the GetMessage in progress")
	case <-time.After(50 * time.Millisecond):
	}
	if r.clock.has(mnsWindow) {
		t.Fatal("window started before the registration succeeded")
	}
	close(gate)
	waitCh(t, mse.regs, "re-registration")
	r.clock.waitTimer(t, mnsWindow)
	eventually(t, "message delivered", func() bool { return len(r.sink.messages()) == 1 })
	r.connectMNS()
	r.waitState(StateReady)
}

func TestMNSLossReRegistrationFails(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) {
		m.regFn = func(n int) byte {
			if n == 1 {
				return obex.RespSuccess
			}
			return obex.RespServiceUnavailable
		}
	}
	mse, phone := r.ready()
	phone.c.Close()
	waitCh(t, mse.regs, "re-registration")
	r.waitState(StateRetrying)
	waitClosed(t, mse.done, "MAS closed")
}

func TestMNSLossWindowExpiryEndsSession(t *testing.T) {
	r := startedRig(t)
	mse, phone := r.ready()
	phone.c.Close()
	waitCh(t, mse.regs, "re-registration")
	r.clock.waitTimer(t, mnsWindow)
	r.clock.Advance(mnsWindow)
	r.waitState(StateRetrying)
	waitClosed(t, mse.done, "MAS closed")
}

func TestMNSWindowTimeout(t *testing.T) {
	r := startedRig(t)
	mse := r.up()
	r.clock.Advance(mnsWindow)
	r.waitState(StateRetrying)
	waitClosed(t, mse.done, "MAS closed")
	if st := r.svc.Status(); !strings.Contains(st.LastError, "notification channel") {
		t.Fatalf("status %+v", st)
	}
	r.clock.waitTimer(t, 10*time.Second)
}

func TestMNSStuckAfterTwoTimeoutsRateLimited(t *testing.T) {
	r := startedRig(t)
	r.svc.PhoneLink(true, r.dialer)
	delays := []time.Duration{settleDelay, 10 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second}
	var calls []int32
	for i, d := range delays {
		r.attemptAfter(d)
		r.clock.Advance(mnsWindow)
		r.waitState(StateRetrying)
		if i+1 < len(delays) {
			r.clock.waitTimer(t, delays[i+1])
		}
		calls = append(calls, r.stuck.Load())
	}
	// Timeouts 1..6: the second calls it; 4 and 5 are inside the 10 minute rate limit; the
	// sixth (630 s after the call) may call it again.
	want := []int32{0, 1, 1, 1, 1, 2}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("stuck calls after timeout %d: got %v want %v", i+1, calls, want)
		}
	}
}

func TestBackoffSequenceAndReset(t *testing.T) {
	r := startedRig(t)
	var fail atomic.Bool
	fail.Store(true)
	r.dialer.find = func(ctx context.Context) (btsdp.MASRecord, error) {
		if fail.Load() {
			return btsdp.MASRecord{}, errors.New("no sdp")
		}
		return btsdp.MASRecord{Channel: 5, MessageTypes: btsdp.MsgTypeSMSGSM}, nil
	}
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	// The failure log line follows arming the retry timer, so a timer of the 300 s kind (also the
	// unavailability timer's length) is unambiguous once its line is there.
	failures := func() []string {
		var out []string
		for _, l := range strings.Split(r.logs.String(), "\n") {
			if strings.Contains(l, "MAP session failed") {
				out = append(out, l)
			}
		}
		return out
	}
	for i, d := range []time.Duration{10, 30, 60, 120, 300, 300} {
		d *= time.Second
		eventually(t, "failure line", func() bool { return len(failures()) == i+1 })
		if l := failures()[i]; !strings.Contains(l, "retry_in="+d.String()) {
			t.Fatalf("failure %d: %s (want %v)", i+1, l, d)
		}
		if i == 5 {
			fail.Store(false)
		}
		r.clock.Advance(d)
	}
	mse := r.nextMSE()
	waitCh(t, mse.regs, "registration")
	r.clock.waitTimer(t, mnsWindow)
	r.connectMNS()
	r.waitState(StateReady)
	// Reaching ready reset the sequence: the next failure waits 10 s again.
	mse.c.Close()
	r.waitState(StateRetrying)
	r.clock.waitTimer(t, 10*time.Second)
}

func TestPermissionHint(t *testing.T) {
	for name, cfg := range map[string]func(*fakeMSE){
		"forbidden":    func(m *fakeMSE) { m.connectCode = obex.RespForbidden },
		"unauthorized": func(m *fakeMSE) { m.connectCode = obex.RespUnauthorized },
		"eof":          func(m *fakeMSE) { m.closeOnConnect = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := startedRig(t)
			r.dialer.cfg = cfg
			r.svc.PhoneLink(true, r.dialer)
			r.clock.waitTimer(t, settleDelay)
			r.clock.Advance(settleDelay)
			r.waitState(StateRetrying)
			if !r.logged("allow message access for siptooth on the phone: Settings → Connected devices → siptooth → Text messages (or answer the phone's message-access prompt)") {
				t.Fatalf("no hint in\n%s", r.logs.String())
			}
		})
	}
}

func TestNoHintForOtherFailures(t *testing.T) {
	r := startedRig(t)
	r.dialer.find = func(ctx context.Context) (btsdp.MASRecord, error) { return btsdp.MASRecord{}, io.EOF }
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	r.waitState(StateRetrying)
	if r.logged("allow message access") {
		t.Fatal("hint for an SDP failure")
	}
}

func TestDedupWithinSessionAndResetAcrossSessions(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) { m.getFn = getOK }
	_, phone := r.ready()
	phone.event("h1")
	phone.event("h1") // duplicate
	phone.event("h2")
	eventually(t, "two messages", func() bool { return len(r.sink.messages()) >= 2 })
	got := r.sink.messages()
	if len(got) != 2 || got[0].Handle != "h1" || got[1].Handle != "h2" {
		t.Fatalf("got %+v", got)
	}
	// A new MAP session starts with an empty window.
	r.svc.PhoneLink(true, r.dialer)
	r.waitState(StateConnecting)
	r.attemptAfter(settleDelay)
	phone2 := r.connectMNS()
	r.waitState(StateReady)
	phone2.event("h1")
	eventually(t, "h1 again", func() bool { return len(r.sink.messages()) == 3 })
}

func TestQueueCapDropsOldest(t *testing.T) {
	r := startedRig(t)
	gate := make(chan struct{})
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(h string) obex.Packet {
			if h == "h1" {
				<-gate
			}
			return getOK(h)
		}
	}
	mse, phone := r.ready()
	phone.event("h1")
	waitCh(t, mse.gets, "first get")
	for i := 2; i <= 40; i++ {
		phone.event("h" + itoa(i))
	}
	close(gate)
	eventually(t, "33 messages", func() bool { return len(r.sink.messages()) == 33 })
	got := r.sink.messages()
	if got[0].Handle != "h1" || got[1].Handle != "h9" || got[32].Handle != "h40" {
		t.Fatalf("handles %v ... %v", got[0].Handle, got[32].Handle)
	}
	if !r.logged("oldest message dropped") {
		t.Fatal("no warning for the dropped handles")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestGetMessageResults(t *testing.T) {
	type tc struct {
		name      string
		fn        func(h string) obex.Packet
		state     string // state after the first event is processed
		delivered int
	}
	tests := []tc{
		{"success", getOK, StateReady, 2},
		{"not found", func(string) obex.Packet { return obex.Packet{Code: obex.RespNotFound} }, StateReady, 0},
		{"bad request", func(string) obex.Packet { return obex.Packet{Code: obex.RespBadRequest} }, StateReady, 0},
		{"not acceptable", func(string) obex.Packet { return obex.Packet{Code: obex.RespNotAcceptable} }, StateReady, 0},
		{"forbidden", func(string) obex.Packet { return obex.Packet{Code: obex.RespForbidden} }, StateReady, 0},
		{"precondition failed", func(string) obex.Packet { return obex.Packet{Code: 0xCC} }, StateReady, 0},
		{"not implemented", func(string) obex.Packet { return obex.Packet{Code: obex.RespNotImplemented} }, StateReady, 0},
		{"parse error", func(string) obex.Packet { return getBody("SECRETBODY") }, StateReady, 0},
		{"unauthorized ends session", func(string) obex.Packet { return obex.Packet{Code: obex.RespUnauthorized} }, StateRetrying, 0},
		{"invalid response code", func(string) obex.Packet { return obex.Packet{Code: 0x10} }, StateRetrying, 0},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			r := startedRig(t)
			r.dialer.cfg = func(m *fakeMSE) { m.getFn = c.fn }
			mse, phone := r.ready()
			phone.event("h1")
			waitCh(t, mse.gets, "get")
			if c.state == StateRetrying {
				r.waitState(StateRetrying)
				waitClosed(t, mse.done, "MAS closed")
				if !r.logged("allow message access") == (c.name == "unauthorized ends session") {
					t.Fatalf("permission hint mismatch:\n%s", r.logs.String())
				}
			} else {
				// The session is kept: a second handle is still fetched and resolved.
				phone.event("h2")
				waitCh(t, mse.gets, "second get")
				eventually(t, "second result resolved", func() bool {
					return len(r.sink.messages()) == c.delivered || strings.Count(r.logs.String(), `msg="message dropped"`) == 2
				})
			}
			if st := r.svc.Status().State; st != c.state {
				t.Fatalf("state %s want %s", st, c.state)
			}
			if n := len(r.sink.messages()); n != c.delivered {
				t.Fatalf("delivered %d want %d", n, c.delivered)
			}
		})
	}
}

func TestGetMessageEOFEndsSession(t *testing.T) {
	r := startedRig(t)
	var mse *fakeMSE
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(string) obex.Packet { m.c.Close(); return obex.Packet{Code: obex.RespSuccess} }
	}
	mse, phone := r.ready()
	phone.event("h1")
	waitCh(t, mse.gets, "get")
	r.waitState(StateRetrying)
	// The real cause of the failed GetMessage, not the generic "closed" from the watcher.
	if got := r.svc.Status().LastError; !strings.Contains(got, "get message") || strings.Contains(got, "MAS connection closed") {
		t.Fatalf("last_error %q", got)
	}
}

func TestIdleMASCloseEndsSession(t *testing.T) {
	r := startedRig(t)
	mse, _ := r.ready()
	mse.c.Close()
	r.waitState(StateRetrying)
	if got := r.svc.Status().LastError; !strings.Contains(got, "MAS connection closed") {
		t.Fatalf("last_error %q", got)
	}
}

func TestSetRegistrationErrorWinsOverServiceError(t *testing.T) {
	r := startedRig(t)
	r.up()
	r.clock.Advance(mnsWindow)
	r.waitState(StateRetrying)
	r.svc.SetRegistrationError(errors.New("boom"))
	if got := r.svc.Status().LastError; got != "MNS profile registration: boom" {
		t.Fatalf("last_error %q", got)
	}
}

func TestGetMessageRetryThenSuccess(t *testing.T) {
	for _, code := range []byte{obex.RespServiceUnavailable, obex.RespInternalError} {
		t.Run(string(rune('A'+code%26)), func(t *testing.T) {
			r := startedRig(t)
			var n atomic.Int32
			r.dialer.cfg = func(m *fakeMSE) {
				m.getFn = func(h string) obex.Packet {
					if n.Add(1) < 3 {
						return obex.Packet{Code: code}
					}
					return getOK(h)
				}
			}
			mse, phone := r.ready()
			phone.event("h1")
			waitCh(t, mse.gets, "attempt 1")
			r.clock.waitTimer(t, getRetryDelay)
			r.clock.Advance(getRetryDelay)
			waitCh(t, mse.gets, "attempt 2")
			r.clock.waitTimer(t, getRetryDelay)
			r.clock.Advance(getRetryDelay)
			waitCh(t, mse.gets, "attempt 3")
			eventually(t, "delivered", func() bool { return len(r.sink.messages()) == 1 })
			if r.svc.Status().State != StateReady {
				t.Fatal(r.svc.Status())
			}
		})
	}
}

func TestGetMessageRetryGivesUpAfterThree(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(string) obex.Packet { return obex.Packet{Code: obex.RespServiceUnavailable} }
	}
	mse, phone := r.ready()
	phone.event("h1")
	for i := 1; i <= 3; i++ {
		waitCh(t, mse.gets, "attempt")
		if i < 3 {
			r.clock.waitTimer(t, getRetryDelay)
			r.clock.Advance(getRetryDelay)
		}
	}
	eventually(t, "dropped", func() bool { return r.logged("message dropped after retries") })
	if r.clock.has(getRetryDelay) || r.svc.Status().State != StateReady || len(r.sink.messages()) != 0 {
		t.Fatalf("pending %v status %+v", r.clock.pending(), r.svc.Status())
	}
	select {
	case h := <-mse.gets:
		t.Fatalf("fourth attempt for %s", h)
	default:
	}
}

func TestPhoneDownDuringGetNeverDelivers(t *testing.T) {
	for name, act := range map[string]func(r *rig){
		"down":     func(r *rig) { r.svc.PhoneLink(false, nil) },
		"replaced": func(r *rig) { r.svc.PhoneLink(true, r.dialer) },
	} {
		t.Run(name, func(t *testing.T) {
			r := startedRig(t)
			gate := make(chan struct{})
			r.dialer.cfg = func(m *fakeMSE) { m.getFn = func(h string) obex.Packet { <-gate; return getOK(h) } }
			mse, phone := r.ready()
			phone.event("h1")
			waitCh(t, mse.gets, "get")
			act(r)
			waitClosed(t, mse.c.Done(), "MAS closed")
			close(gate)
			eventually(t, "stale result seen", func() bool {
				return r.logged("message result discarded") || r.logged("message lost with the MAP session")
			})
			if n := len(r.sink.messages()); n != 0 {
				t.Fatalf("delivered %d", n)
			}
			if !r.logged("handle=h1") {
				t.Fatal("lost handle not logged")
			}
		})
	}
}

func TestMNSAdmissionFullInboxAndAfterShutdown(t *testing.T) {
	r := newRig(t) // not running: the inbox fills up
	for i := 0; i < inboxSize; i++ {
		r.svc.PhoneLink(false, nil)
	}
	a, b := net.Pipe()
	r.svc.MNSConnection(a)
	_ = b.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("full inbox: connection not closed: %v", err)
	}

	r2 := startedRig(t)
	r2.stop()
	a2, b2 := net.Pipe()
	r2.svc.MNSConnection(a2)
	_ = b2.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := b2.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("after shutdown: connection not closed: %v", err)
	}
	r2.svc.PhoneLink(true, r2.dialer) // returns without blocking
	r2.svc.MNSDisconnected()
}

func TestMNSDisconnectedClosesBeforeReturning(t *testing.T) {
	r := newRig(t)
	a, b := net.Pipe()
	r.svc.MNSConnection(a)
	r.svc.MNSDisconnected()
	_ = b.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection not closed on return: %v", err)
	}
}

func TestMNSReplacedClosesOld(t *testing.T) {
	r := startedRig(t)
	mse, phone1 := r.ready()
	phone2 := r.connectMNS()
	waitClosed(t, phone1.c.Done(), "old MNS closed")
	waitCh(t, mse.regs, "re-registration after the replacement")
	r.waitState(StateReady)
	if !r.svc.Status().Enabled || !*r.svc.Status().Notifications {
		t.Fatal(r.svc.Status())
	}
	_ = phone2
}

func TestEventsWithoutMASAreDropped(t *testing.T) {
	r := startedRig(t)
	phone := r.connectMNS()
	eventually(t, "notifications open", func() bool { return *r.svc.Status().Notifications })
	phone.event("h1")
	eventually(t, "drop logged", func() bool { return r.logged("no MAP session") && r.logged("handle=h1") })
	if len(r.sink.messages()) != 0 {
		t.Fatal("delivered without a session")
	}
	if r.svc.Status().State != StateWaiting {
		t.Fatal(r.svc.Status())
	}
}

func TestEventFiltering(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) { m.getFn = getOK }
	_, phone := r.ready()
	put := func(typ, handle, folder, msgType string, inst []byte) {
		xml := `<MAP-event-report version="1.0"><event type="` + typ + `" handle="` + handle + `" folder="` + folder + `" msg_type="` + msgType + `"/></MAP-event-report>`
		var ap []byte
		if inst != nil {
			ap = obex.EncodeAppParams(obex.AppParam{Tag: 0x0F, Value: inst})
		}
		phone.putEvent(typeEventReport, ap, xml)
	}
	put("NewMessage", "mms", "TELECOM/MSG/INBOX", "MMS", nil)
	put("NewMessage", "sent", "TELECOM/MSG/SENT", "SMS_GSM", nil)
	put("MessageDeleted", "del", "TELECOM/MSG/INBOX", "SMS_GSM", nil)
	put("NewMessage", "other", "TELECOM/MSG/INBOX", "SMS_GSM", []byte{9})
	put("NewMessage", "good", "telecom/msg/inbox", "SMS_CDMA", []byte{0})
	eventually(t, "good delivered", func() bool { return len(r.sink.messages()) >= 1 })
	if got := r.sink.messages(); len(got) != 1 || got[0].Handle != "good" {
		t.Fatalf("got %+v", got)
	}
	if !r.logged("MMS notification ignored") {
		t.Fatal("MMS not logged at info")
	}
}

func TestDeliveredMessageAndCounter(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(string) obex.Packet { return getBody(bmsgText("+380501234567", "Jane Doe", "Hello\nworld")) }
	}
	_, phone := r.ready()
	phone.event("h1")
	eventually(t, "received 1", func() bool { return *r.svc.Status().Received == 1 })
	m := r.sink.messages()[0]
	if m.ID != "id-1" || m.Handle != "h1" || m.Type != "SMS_GSM" || m.From != "+380501234567" || m.FromName != "Jane Doe" || m.Text != "Hello\nworld" {
		t.Fatalf("got %+v", m)
	}
	// A message the dispatcher refuses is not counted.
	r.sink.mu.Lock()
	r.sink.reject = true
	r.sink.mu.Unlock()
	phone.event("h2")
	eventually(t, "second offered", func() bool { return len(r.sink.messages()) == 2 })
	eventually(t, "warning", func() bool { return r.logged("not accepted") })
	if n := *r.svc.Status().Received; n != 1 {
		t.Fatalf("received %d", n)
	}
	// The counter survives sessions.
	r.svc.PhoneLink(false, nil)
	r.waitState(StateWaiting)
	if n := *r.svc.Status().Received; n != 1 {
		t.Fatalf("received %d after the session ended", n)
	}
}

func TestUnavailableAfterFiveMinutesOnceAndRearmed(t *testing.T) {
	r := startedRig(t)
	var fail atomic.Bool
	fail.Store(true)
	r.dialer.find = func(ctx context.Context) (btsdp.MASRecord, error) {
		if fail.Load() {
			return btsdp.MASRecord{}, errors.New("sdp down")
		}
		return btsdp.MASRecord{Channel: 5, MessageTypes: btsdp.MsgTypeSMSGSM}, nil
	}
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, unavailableAfter)
	r.clock.Advance(4 * time.Minute) // not yet
	r.waitState(StateRetrying)
	if len(r.sink.unavailable()) != 0 {
		t.Fatal("early")
	}
	r.clock.waitTimer(t, unavailableAfter)
	r.clock.Advance(time.Minute)
	eventually(t, "unavailable", func() bool { return len(r.sink.unavailable()) == 1 })
	if d := r.sink.unavailable()[0]; !strings.Contains(d, "sdp down") {
		t.Fatalf("detail %q", d)
	}
	// Once only, however long it stays down.
	for i := 0; i < 5; i++ {
		r.clock.Advance(5 * time.Minute)
	}
	r.waitState(StateRetrying)
	if n := len(r.sink.unavailable()); n != 1 {
		t.Fatalf("emitted %d times", n)
	}
	// Reaching ready re-arms it; a long outage afterwards reports again.
	fail.Store(false)
	deadline := time.Now().Add(waitFor)
	for r.svc.Status().State != StateReady {
		if time.Now().After(deadline) {
			t.Fatalf("never ready: %+v pending %v", r.svc.Status(), r.clock.pending())
		}
		select {
		case mse := <-r.dialer.mses:
			waitCh(t, mse.regs, "registration")
			r.clock.waitTimer(t, mnsWindow)
			r.connectMNS()
			r.waitState(StateReady)
		default:
			if p := r.clock.pending(); len(p) > 0 {
				r.clock.Advance(p[0])
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}
	if r.clock.has(unavailableAfter) {
		t.Fatal("timer still armed when ready")
	}
	fail.Store(true)
	r.svc.PhoneLink(true, r.dialer) // replace: down again
	r.clock.waitTimer(t, unavailableAfter)
	r.clock.Advance(unavailableAfter)
	eventually(t, "second report", func() bool { return len(r.sink.unavailable()) == 2 })
}

func TestNoUnavailableWhenReadyQuickly(t *testing.T) {
	r := startedRig(t)
	r.ready()
	r.clock.Advance(time.Hour)
	// The hourly power cycle: link down and up, ready again within seconds.
	r.svc.PhoneLink(false, nil)
	r.waitState(StateWaiting)
	if r.clock.has(unavailableAfter) {
		t.Fatal("timer armed while the phone is down")
	}
	r.svc.PhoneLink(true, r.dialer)
	r.attemptAfter(settleDelay)
	r.connectMNS()
	r.waitState(StateReady)
	r.clock.Advance(time.Hour)
	if n := len(r.sink.unavailable()); n != 0 {
		t.Fatalf("emitted %d", n)
	}
}

func TestShutdownWithBlockedWorkers(t *testing.T) {
	old := shutdownWait
	shutdownWait = 100 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })
	r := startedRig(t)
	entered, block := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(block) })
	r.dialer.find = func(ctx context.Context) (btsdp.MASRecord, error) { // ignores ctx
		close(entered)
		<-block
		return btsdp.MASRecord{}, errors.New("late")
	}
	r.svc.PhoneLink(true, r.dialer)
	r.clock.waitTimer(t, settleDelay)
	r.clock.Advance(settleDelay)
	waitClosed(t, entered, "FindMAS")
	start := time.Now()
	r.cancel()
	waitClosed(t, r.done, "Run to return")
	if time.Since(start) > 2*time.Second {
		t.Fatal("shutdown took too long")
	}
	r.svc.PhoneLink(false, nil)
}

func TestShutdownClosesConnections(t *testing.T) {
	r := startedRig(t)
	mse, phone := r.ready()
	r.cancel()
	waitClosed(t, r.done, "Run")
	waitClosed(t, mse.done, "MAS closed")
	waitClosed(t, phone.c.Done(), "MNS closed")
}

func TestLogsNeverContainMessageData(t *testing.T) {
	r := startedRig(t)
	r.dialer.cfg = func(m *fakeMSE) {
		m.getFn = func(h string) obex.Packet {
			if h == "bad" {
				return getBody("SECRETBODY +1999 SECRETNAME")
			}
			return getBody(bmsgText("+380501234567", "SECRETNAME", "SECRETTEXT"))
		}
	}
	_, phone := r.ready()
	phone.event("h1")
	phone.event("bad")
	phone.putEvent(typeEventReport, nil, "SECRETEVENT<")
	phone.putEvent(typeEventReport, nil, `<MAP-event-report version="1.0"><event type="NewMessage" handle="h3" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM" subject="SECRETSUBJECT"/></MAP-event-report>`)
	eventually(t, "two messages and a drop", func() bool {
		return len(r.sink.messages()) == 2 && r.logged("message dropped") && r.logged("unreadable")
	})
	r.svc.PhoneLink(false, nil)
	r.waitState(StateWaiting)
	for _, s := range []string{"SECRETTEXT", "SECRETNAME", "SECRETBODY", "SECRETEVENT", "SECRETSUBJECT", "+380501234567"} {
		if r.logged(s) {
			t.Errorf("log contains %q:\n%s", s, r.logs.String())
		}
	}
}

func TestDisabled(t *testing.T) {
	var sink fakeSink
	svc := NewService(Deps{Enabled: false, Sink: &sink, Clock: newFakeClock()})
	b, err := json.Marshal(svc.Status())
	if err != nil || string(b) != `{"enabled":false,"state":"disabled"}` {
		t.Fatalf("%s %v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	d := &fakeDialer{t: t, mses: make(chan *fakeMSE, 1)}
	svc.PhoneLink(true, d)
	a, bb := net.Pipe()
	svc.MNSConnection(a)
	_ = bb.SetReadDeadline(time.Now().Add(waitFor))
	if _, err := bb.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("disabled service kept the connection: %v", err)
	}
	svc.MNSDisconnected()
	cancel()
	waitClosed(t, done, "Run")
	if st := svc.Status(); st.State != StateDisabled || st.Notifications != nil || st.Received != nil {
		t.Fatalf("%+v", st)
	}
}

func TestStatusJSONEnabled(t *testing.T) {
	r := newRig(t)
	b, _ := json.Marshal(r.svc.Status())
	if string(b) != `{"enabled":true,"state":"waiting","notifications":false,"received":0}` {
		t.Fatalf("%s", b)
	}
}

func TestSetRegistrationError(t *testing.T) {
	r := newRig(t)
	r.svc.SetRegistrationError(errors.New("boom"))
	if got := r.svc.Status().LastError; !strings.Contains(got, "boom") {
		t.Fatalf("%q", got)
	}
	r.svc.SetRegistrationError(nil)
	if got := r.svc.Status().LastError; got != "" {
		t.Fatalf("%q", got)
	}
}

func TestLastErrorClearedOnReady(t *testing.T) {
	r := startedRig(t)
	r.up()
	r.clock.Advance(mnsWindow)
	r.waitState(StateRetrying)
	if r.svc.Status().LastError == "" {
		t.Fatal("no last_error after a failure")
	}
	r.attemptAfter(10 * time.Second)
	r.connectMNS()
	r.waitState(StateReady)
	if got := r.svc.Status().LastError; got != "" {
		t.Fatalf("last_error %q not cleared", got)
	}
}
