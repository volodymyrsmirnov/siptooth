package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/nat"
	"github.com/volodymyrsmirnov/siptooth/internal/rtp"
	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

const (
	// stunQueryTimeout and stunQueryAttempts are the per-server STUN policy
	// of spec 7.2.
	stunQueryTimeout  = 500 * time.Millisecond
	stunQueryAttempts = 2
	// stunKeepaliveEvery re-sends a Binding Request while the call is dialing
	// (spec 7.2).
	stunKeepaliveEvery = 10 * time.Second
	// stunFailedEvery rate-limits the stun_failed system.error (spec 7.2).
	stunFailedEvery = time.Hour

	defaultSIPPort = 5060
)

// SIPClient is the part of *sip.UA the adapter uses.
type SIPClient interface {
	Start(req sip.DialRequest) (sip.Attempt, error)
	ResolveNextHop(ctx context.Context) (netip.AddrPort, error)
	DiscoverContact(ctx context.Context, target string) (netip.AddrPort, bool)
}

// ErrorSink receives system.error events (*hooks.Dispatcher).
type ErrorSink interface {
	EmitError(code, detail string)
}

// AdapterDeps are the SIPAdapter's collaborators.
type AdapterDeps struct {
	UA     SIPClient
	Config config.Config
	// Selector decides the advertised media address; nil builds one from
	// Config.NAT.
	Selector *nat.Selector
	// Registration returns the live registration state; ok is false when
	// there is no registration (sip.register off, or not started yet).
	Registration func() (state sip.RegState, ok bool)
	Hooks        ErrorSink
	Clock        Clock
	Logger       *slog.Logger

	// STUNTimeout and STUNKeepalive override the spec values (500 ms, 10 s);
	// tests shorten them.
	STUNTimeout   time.Duration
	STUNKeepalive time.Duration
}

// SIPAdapter implements SIP on top of a sip.UA, rtp.Listen and nat.Selector.
type SIPAdapter struct {
	d   AdapterDeps
	sel *nat.Selector

	mu          sync.Mutex
	lastSTUNErr time.Time
}

// NewSIPAdapter returns the adapter the daemon hands to the coordinator.
func NewSIPAdapter(d AdapterDeps) *SIPAdapter {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.STUNTimeout <= 0 {
		d.STUNTimeout = stunQueryTimeout
	}
	if d.STUNKeepalive <= 0 {
		d.STUNKeepalive = stunKeepaliveEvery
	}
	sel := d.Selector
	if sel == nil {
		pub, _ := netip.ParseAddr(d.Config.NAT.PublicIP)
		sel = nat.NewSelector(nat.Mode(d.Config.NAT.Mode), d.Config.NAT.StunServers, pub)
	}
	return &SIPAdapter{d: d, sel: sel}
}

// rtpMedia is the call's *rtp.Session as bridge.Media: Start takes the
// jitter configuration from config.
type rtpMedia struct {
	*rtp.Session
	cfg rtp.Config
}

func (m rtpMedia) Start(md sdp.Media, localKey []byte) error {
	return m.Session.Start(md, localKey, m.cfg)
}

// Prepare opens the RTP socket toward nextHopHost, decides the advertised
// media address, starts the STUN keepalive for stun mode and works out the
// Contact (spec 4.2 step 4, 7).
func (a *SIPAdapter) Prepare(ctx context.Context, nextHopHost string) (Prepared, error) {
	cfg := a.d.Config
	hop, err := a.resolveHop(ctx, nextHopHost)
	if err != nil {
		return Prepared{}, err
	}
	bind, err := a.bindAddr(hop.Addr())
	if err != nil {
		return Prepared{}, err
	}
	sess, err := rtp.Listen(bind, cfg.NAT.RTPPortMin, cfg.NAT.RTPPortMax)
	if err != nil {
		return Prepared{}, err
	}
	media := rtpMedia{Session: sess, cfg: rtp.Config{
		JitterMs: cfg.Media.JitterMs, SilenceRMS: cfg.Media.SilenceRMS,
		StartupCapSeconds: 20, HardCapSeconds: 20,
	}}
	fail := func(err error) (Prepared, error) {
		_ = sess.Close()
		return Prepared{}, err
	}

	query := func(server string) (netip.AddrPort, error) {
		if err := ctx.Err(); err != nil {
			return netip.AddrPort{}, err
		}
		return sess.QuerySTUN(server, a.d.STUNTimeout, stunQueryAttempts)
	}
	dec := a.sel.Decide(query, sess.LocalAddr(), hop.Addr())
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if dec.Warning != "" {
		a.d.Logger.Warn("nat: "+dec.Warning, "mode", string(dec.Mode), "advertised", dec.Media.String())
	}
	if dec.STUNFailed {
		a.noteSTUNFailed(dec.Warning)
	}
	if dec.Mode == nat.ModeSTUN && dec.STUNServer != "" {
		log := a.d.Logger
		sess.StartSTUNKeepalive(dec.STUNServer, a.d.STUNKeepalive, func(ap netip.AddrPort) {
			log.Warn("nat: STUN mapping changed while dialing", "mapped", ap.String())
		})
	}

	offer := sdp.Offer{
		SessID: uint64(a.d.Clock.Now().Unix()), Version: 1,
		Addr: dec.Media,
	}
	for _, name := range cfg.Media.Codecs {
		codec, err := g711.ParseCodec(name)
		if err != nil {
			return fail(fmt.Errorf("media codec: %w", err))
		}
		offer.Codecs = append(offer.Codecs, codec)
	}
	var key []byte
	if cfg.SIP.SRTP {
		if key, err = sdp.NewKey(); err != nil {
			return fail(err)
		}
		offer.LocalKey = key
	}

	contact := a.contact(ctx, nextHopHost, dec)
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return Prepared{Media: media, Offer: offer, LocalKey: key, Contact: contact, NAT: dec}, nil
}

// Start sends the INVITE.
func (a *SIPAdapter) Start(req sip.DialRequest) (sip.Attempt, error) { return a.d.UA.Start(req) }

// resolveHop returns the IP the INVITE goes to: a literal host as is, else
// the UA's own resolution of its next hop.
func (a *SIPAdapter) resolveHop(ctx context.Context, host string) (netip.AddrPort, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return netip.AddrPortFrom(ip.Unmap(), 0), nil
	}
	return a.d.UA.ResolveNextHop(ctx)
}

// bindAddr is nat.rtp_bind, else the local address the kernel picks toward
// the next hop (spec 7.1).
func (a *SIPAdapter) bindAddr(hop netip.Addr) (netip.Addr, error) {
	if b := a.d.Config.NAT.RTPBind; b != "" {
		ip, err := netip.ParseAddr(b)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("nat.rtp_bind %q: %w", b, err)
		}
		return ip, nil
	}
	local, err := nat.LocalAddrToward(hop)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("local address toward %s: %w", hop, err)
	}
	return local, nil
}

// noteSTUNFailed logs and emits system.error stun_failed at most once per
// hour (spec 7.2).
func (a *SIPAdapter) noteSTUNFailed(detail string) {
	now := a.d.Clock.Now()
	a.mu.Lock()
	due := a.lastSTUNErr.IsZero() || now.Sub(a.lastSTUNErr) >= stunFailedEvery
	if due {
		a.lastSTUNErr = now
	}
	a.mu.Unlock()
	if due && a.d.Hooks != nil {
		a.d.Hooks.EmitError("stun_failed", detail)
	}
}

// contact works out the signaling address (spec 7.3): the registered flow
// address while registered, the local address in local mode, else what the
// next hop sees (OPTIONS), else the media address IP.
func (a *SIPAdapter) contact(ctx context.Context, host string, dec nat.Decision) string {
	cfg := a.d.Config.SIP
	transport := strings.ToLower(cfg.Transport)
	user := "siptooth"
	if cfg.Register {
		user = cfg.Username
		if a.d.Registration != nil {
			if st, ok := a.d.Registration(); ok && st.Contact.IsValid() {
				return ContactURI(user, st.Contact, transport)
			}
		}
	}
	port := uint16(cfg.LocalPort)
	if port == 0 {
		port = defaultSIPPort
	}
	if dec.Mode == nat.ModeLocal {
		return ContactURI(user, netip.AddrPortFrom(dec.Local, port), transport)
	}
	target := "sip:" + host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		target = "sip:[" + host + "]"
	}
	if ap, ok := a.d.UA.DiscoverContact(ctx, target); ok {
		return ContactURI(user, ap, transport)
	}
	return ContactURI(user, netip.AddrPortFrom(dec.Media.Addr(), port), transport)
}

// ContactURI renders sip:<user>@<addr>;transport=<t> (spec 5.2).
func ContactURI(user string, addr netip.AddrPort, transport string) string {
	return fmt.Sprintf("sip:%s@%s;transport=%s", user, net.JoinHostPort(addr.Addr().String(), fmt.Sprint(addr.Port())), transport)
}
