package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/nat"
)

// registerRetryEvery is the wait before a failed REGISTER is retried (spec 5.7).
var registerRetryEvery = 30 * time.Second

// idleKeepaliveEvery is the CRLF ping interval of the registration flow
// between calls (spec 5.5, 5.7).
// registerMinRefresh floors the refresh interval, whatever the registrar grants.
var registerMinRefresh = 5 * time.Second

var idleKeepaliveEvery = 25 * time.Second

const (
	defaultRegisterExpires = 120 * time.Second
	registerTxTimeout      = 20 * time.Second
	maxRegisterChallenges  = 2 // as for INVITE (spec 5.4)
	maxFlowFollowups       = 3 // immediate re-registrations per cycle
	defaultSIPPort         = 5060
)

// RegState is the registration state handed to onState and State.
type RegState struct {
	Registered bool
	// Contact is the flow address the registrar saw (Via received/rport of
	// its last answer); before the first answer, the best local guess. The
	// adapter uses it as Contact for INVITEs while registered (spec 7.3).
	Contact netip.AddrPort
	// Expires is when the binding granted by the last 2xx lapses.
	Expires time.Time
	// Err is why the last attempt failed; nil when Registered.
	Err error
}

// Registration keeps one binding alive at a registrar until its context ends
// or Unregister is called (spec 5.7).
type Registration struct {
	u         *UA
	aor       gosip.Uri
	registrar gosip.Uri
	want      time.Duration
	onState   func(RegState)
	tp        string // upper case Via/transport name

	// Dialog identifiers are fixed for the registration (RFC 3261 10.2).
	callID  string
	fromTag string

	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	state RegState

	// Owned by the run goroutine; Unregister uses them only after it exited.
	cseq    uint32
	contact netip.AddrPort   // the Contact to register next
	held    []netip.AddrPort // Contacts this UA may have bound at the registrar
	sent    bool             // a REGISTER was sent
}

// Register starts keeping aor registered at registrarHost ("host" or
// "host:port") until ctx ends. aor "" means sip:<username>@<registrarHost>.
// The REGISTER goes to the UA's next hop on the UA's transport, so the INVITEs
// that follow share its connection or UDP flow. expires is the requested
// binding lifetime; the registrar's grant (Contact expires parameter, else
// Expires header) is refreshed at half its value. Failures are retried every
// 30 s. onState (may be nil) is called, in order, on every state change, from
// the registration's goroutine: it must not call Unregister (which waits for
// that goroutine) or block for long.
//
// A UA keeps one registration. Calling Register while another is active is
// not supported: the returned Registration is dead, with State().Err set (and
// onState called with it), and the active one is untouched.
func (u *UA) Register(ctx context.Context, aor, registrarHost string, expires time.Duration, onState func(RegState)) *Registration {
	if expires <= 0 {
		expires = defaultRegisterExpires
	}
	if aor == "" {
		aor = "sip:" + u.opts.Username + "@" + registrarHost
	}
	r := &Registration{
		u: u, want: expires, onState: onState, tp: strings.ToUpper(u.transport),
		callID:  gosip.GenerateTagN(24),
		fromTag: gosip.GenerateTagN(16),
		done:    make(chan struct{}),
	}
	rctx, cancel := context.WithCancel(u.bgCtx)
	stop := context.AfterFunc(ctx, cancel)
	r.cancel = func() { stop(); cancel() }

	if err := gosip.ParseUri(aor, &r.aor); err != nil || r.aor.Host == "" {
		r.fail(fmt.Errorf("sip: registration AOR %q: invalid", aor))
		return r
	}
	if err := gosip.ParseUri("sip:"+registrarHost, &r.registrar); err != nil || r.registrar.Host == "" {
		r.fail(fmt.Errorf("sip: registrar %q: invalid", registrarHost))
		return r
	}
	if !u.reg.CompareAndSwap(nil, r) {
		r.fail(errors.New("sip: a registration is already active on this UA"))
		return r
	}
	if !u.spawn(func() { r.run(rctx) }) {
		u.reg.CompareAndSwap(r, nil)
		r.fail(errors.New("sip: user agent closed"))
	}
	return r
}

// fail ends a registration that never started.
func (r *Registration) fail(err error) {
	r.cancel()
	close(r.done)
	r.setState(RegState{Err: err})
}

// State is the current state.
func (r *Registration) State() RegState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Registration) setState(s RegState) {
	r.mu.Lock()
	changed := !sameState(r.state, s)
	r.state = s
	r.mu.Unlock()
	if changed && r.onState != nil {
		r.onState(s)
	}
}

func sameState(a, b RegState) bool {
	ae, be := "", ""
	if a.Err != nil {
		ae = a.Err.Error()
	}
	if b.Err != nil {
		be = b.Err.Error()
	}
	return a.Registered == b.Registered && a.Contact == b.Contact && a.Expires.Equal(b.Expires) && ae == be
}

func (r *Registration) run(ctx context.Context) {
	defer close(r.done)
	u := r.u
	defer u.reg.CompareAndSwap(r, nil)
	var keep <-chan time.Time
	if u.idleKeepalive > 0 {
		t := time.NewTicker(u.idleKeepalive)
		defer t.Stop()
		keep = t.C
	}
	var wait time.Duration // the first REGISTER goes out at once
	for {
		timer := time.NewTimer(wait)
	idle:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				break idle
			case <-keep:
				if err := r.keepalive(ctx); err != nil && ctx.Err() == nil {
					u.log.Warn("registration keepalive failed, registering again", "error", err)
					timer.Stop()
					break idle
				}
			}
		}
		granted, err := r.cycle(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			wait = u.regRetry
			continue
		}
		wait = u.refreshAfter(granted)
	}
}

// refreshAfter is the wait before the next REGISTER: half the granted
// lifetime, but not under the UA's floor (so a tiny grant cannot make a busy
// loop), and always before the binding lapses.
func (u *UA) refreshAfter(granted time.Duration) time.Duration {
	wait := granted / 2
	if wait < u.regMinRefresh {
		wait = min(u.regMinRefresh, granted*3/4)
	}
	return wait
}

// keepalive sends the idle CRLF ping on the registration flow. While a call
// is up its own keepalive holds the same flow (INVITE and REGISTER share the
// next hop).
func (r *Registration) keepalive(ctx context.Context) error {
	u := r.u
	u.mu.Lock()
	busy := len(u.calls) > 0
	u.mu.Unlock()
	if busy {
		return nil // the call's own keepalive rides the same flow
	}
	return u.sendKeepalive(ctx)
}

// cycle registers once and, when the registrar saw a different flow address
// than the Contact it was given, again at once with the discovered one (the
// same request expires the superseded Contact). It returns the granted
// lifetime. Registered is published only once the flow is settled (or the follow-up cap
// is reached, then with the last registered Contact), so a caller that dials
// on it never gets a pre-correction Contact.
func (r *Registration) cycle(ctx context.Context) (time.Duration, error) {
	if !r.contact.IsValid() {
		c, err := r.initialContact(ctx)
		if err != nil {
			r.setFailed(err)
			return 0, err
		}
		r.contact = c
	}
	var granted time.Duration
	var last RegState // the last Contact successfully registered
	for i := 0; i < maxFlowFollowups; i++ {
		used := r.contact
		res, flow, err := r.register(ctx, used, r.want)
		if err != nil {
			if ctx.Err() == nil {
				r.u.log.Warn("registration failed", "error", err)
				r.setFailed(err)
			}
			return 0, err
		}
		granted = grantedLifetime(res, used, r.aor.User, r.want)
		if granted <= 0 {
			err := errors.New("sip: registrar granted no lifetime")
			r.setFailed(err)
			return 0, err
		}
		r.held = []netip.AddrPort{used} // the same request expired every other Contact
		last = RegState{Registered: true, Contact: used, Expires: time.Now().Add(granted)}
		if !flow.IsValid() || flow == used {
			r.setState(last) // the flow is settled: only now is the Contact worth dialing with
			return granted, nil
		}
		r.contact = flow
		r.u.log.Info("registrar sees a different flow address, registering again", "contact", flow.String())
	}
	r.u.log.Warn("flow address keeps changing, registering the newest one at the next refresh", "contact", r.contact.String())
	r.setState(last)
	return granted, nil
}

func (r *Registration) setFailed(err error) {
	r.setState(RegState{Contact: r.contact, Err: err})
}

// initialContact is the best guess before the registrar has answered: the
// local address toward the next hop with the configured local SIP port.
func (r *Registration) initialContact(ctx context.Context) (netip.AddrPort, error) {
	hop, err := r.u.ResolveNextHop(ctx)
	if err != nil {
		return netip.AddrPort{}, err
	}
	local, err := nat.LocalAddrToward(hop.Addr())
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("sip: local address toward %s: %w", hop.Addr(), err)
	}
	if r.u.transport != "udp" {
		// The ephemeral local port exists only once the connection does:
		// open it with an OPTIONS probe (its answer is not needed).
		r.u.DiscoverContactReason(ctx, r.registrar.String())
	}
	port := uint16(r.u.opts.LocalPort)
	if ap, ok := r.u.LocalAddr(); ok {
		port = ap.Port()
	}
	if port == 0 {
		port = defaultSIPPort
	}
	return netip.AddrPortFrom(local, port), nil
}

// contactURI is the Contact for the flow address c.
func (r *Registration) contactURI(c netip.AddrPort) gosip.Uri {
	uri := gosip.Uri{Scheme: "sip", User: r.aor.User, Host: c.Addr().String(), Port: int(c.Port())}
	if r.u.transport != "udp" {
		uri.UriParams = gosip.HeaderParams{{K: "transport", V: r.u.transport}}
	}
	return uri
}

// register sends one REGISTER for contact with the requested lifetime and
// answers digest challenges. It returns the 2xx and the flow address from the
// top Via of the answer (of the challenge when the 2xx has none: a digest
// retry over TCP is built without rport by sipgo, on the same connection).
func (r *Registration) register(ctx context.Context, contact netip.AddrPort, lifetime time.Duration) (*gosip.Response, netip.AddrPort, error) {
	u := r.u
	ctx, cancel := context.WithTimeout(ctx, registerTxTimeout)
	defer cancel()

	dest, err := u.destination(ctx, u.transport, u.opts.NextHop)
	if err != nil {
		return nil, netip.AddrPort{}, fmt.Errorf("sip: REGISTER next hop: %w", err)
	}
	req := gosip.NewRequest(gosip.REGISTER, r.registrar)
	req.SetTransport(r.tp)
	req.SetDestination(dest)

	// rport on every transport, as for contact discovery (RFC 3581).
	via := &gosip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: r.tp}
	via.Params.Add("branch", gosip.GenerateBranchN(16))
	via.Params.Add("rport", "")
	req.AppendHeader(via)

	from := &gosip.FromHeader{Address: *r.aor.Clone()}
	from.Params.Add("tag", r.fromTag)
	req.AppendHeader(from)
	req.AppendHeader(&gosip.ToHeader{Address: *r.aor.Clone()})
	callID := gosip.CallIDHeader(r.callID)
	req.AppendHeader(&callID)
	r.cseq++
	req.AppendHeader(&gosip.CSeqHeader{SeqNo: r.cseq, MethodName: gosip.REGISTER})
	maxFwd := gosip.MaxForwardsHeader(70)
	req.AppendHeader(&maxFwd)
	ch := &gosip.ContactHeader{Address: r.contactURI(contact)}
	secs := int(lifetime / time.Second)
	if secs == 0 {
		ch.Params.Add("expires", "0") // an unregister names it on the Contact too
	}
	req.AppendHeader(ch)
	// Every other Contact this UA may have bound is expired by the same
	// request, so a registrar that keeps one binding per AOR never holds two.
	r.held = appendUnique(r.held, contact)
	for _, old := range r.held {
		if old == contact {
			continue
		}
		oc := &gosip.ContactHeader{Address: r.contactURI(old)}
		oc.Params.Add("expires", "0")
		req.AppendHeader(oc)
	}
	req.AppendHeader(gosip.NewHeader("Expires", strconv.Itoa(secs)))
	req.AppendHeader(gosip.NewHeader("Allow", allowMethods))
	req.AppendHeader(gosip.NewHeader("User-Agent", u.opts.UserAgent))

	LogMessage(u.log, "send", req)
	r.sent = true
	res, err := u.client.Do(ctx, req)
	if err != nil {
		u.forgetHop(u.opts.NextHop)
		return nil, netip.AddrPort{}, fmt.Errorf("sip: REGISTER: %w", err)
	}
	var challengeFlow netip.AddrPort
	challenges := 0
	for {
		switch {
		case res.IsSuccess():
			flow, ok := viaFlow(res)
			if !ok {
				flow = challengeFlow
			}
			if cs := req.CSeq(); cs != nil {
				r.cseq = cs.SeqNo
			}
			return res, flow, nil
		case res.StatusCode == 423 && secs > 0:
			if min := minExpires(res); min > lifetime {
				r.want = min // the registrar's floor
			}
			return nil, netip.AddrPort{}, fmt.Errorf("sip: REGISTER answered %d %s", res.StatusCode, res.Reason)
		case isChallenge(res):
			if f, ok := viaFlow(res); ok && !challengeFlow.IsValid() {
				challengeFlow = f
			}
			if u.opts.Username == "" || u.opts.Password == "" {
				return nil, netip.AddrPort{}, errors.New("sip: REGISTER challenged: auth_required")
			}
			if challenges >= maxRegisterChallenges {
				return nil, netip.AddrPort{}, errors.New("sip: REGISTER challenged again: auth_failed")
			}
			challenges++
			res, err = u.client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
				Username: u.opts.Username, Password: u.opts.Password,
			})
			if err != nil {
				return nil, netip.AddrPort{}, fmt.Errorf("sip: REGISTER digest: %w", err)
			}
		default:
			return nil, netip.AddrPort{}, fmt.Errorf("sip: REGISTER answered %d %s", res.StatusCode, res.Reason)
		}
	}
}

func appendUnique(s []netip.AddrPort, v netip.AddrPort) []netip.AddrPort {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func minExpires(res *gosip.Response) time.Duration {
	if h := res.GetHeader("Min-Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// grantedLifetime reads the binding lifetime out of a 2xx: the expires
// parameter of our Contact (or of the only Contact), else the Expires header,
// else what was asked.
func grantedLifetime(res *gosip.Response, ours netip.AddrPort, user string, asked time.Duration) time.Duration {
	var contacts []*gosip.ContactHeader
	for _, h := range res.GetHeaders("Contact") {
		if c, ok := h.(*gosip.ContactHeader); ok {
			contacts = append(contacts, c)
		}
	}
	paramOf := func(c *gosip.ContactHeader) (time.Duration, bool) {
		v, ok := c.Params.Get("expires")
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	for _, c := range contacts {
		host, port := c.Address.Host, c.Address.Port
		if c.Address.User == user && net.JoinHostPort(host, strconv.Itoa(port)) == ours.String() {
			if d, ok := paramOf(c); ok {
				return d
			}
		}
	}
	if len(contacts) == 1 {
		if d, ok := paramOf(contacts[0]); ok {
			return d
		}
	}
	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return asked
}

// Unregister stops the registration and removes every Contact this UA
// registered (never Contact: *) with Expires: 0, bounded by ctx. It does
// nothing when no REGISTER was ever sent. Safe to call more than once. It
// waits for the registration's goroutine, so it must not be called from the
// onState callback.
func (r *Registration) Unregister(ctx context.Context) error {
	r.cancel()
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.u.reg.CompareAndSwap(r, nil)
	if !r.sent || !r.contact.IsValid() {
		return nil
	}
	// register names every held Contact besides r.contact with expires=0 too.
	_, _, err := r.register(ctx, r.contact, 0)
	if err == nil {
		r.sent, r.held = false, nil
	}
	r.setState(RegState{Contact: r.contact, Err: err})
	return err
}
