// Package sip wraps emiago/sipgo for siptooth: one user agent that hosts the
// client, the server for far-end requests that arrive on connections siptooth
// opened, TLS and write-timeout policy, Contact discovery and keepalive
// (spec sections 5.2, 5.5, 7.3).
package sip

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/nat"
)

// tcpWriteTimeout bounds every TCP and TLS write (spec 5.2), so teardown is
// bounded even on a stuck connection. sipgo applies it as a write deadline
// on each connection it creates. A variable only so tests can shorten it.
var tcpWriteTimeout = 5 * time.Second

// Options configures a UA.
type Options struct {
	Transport string // udp|tcp|tls
	NextHop   string // host:port (sip.proxy or target host:port)
	LocalPort int    // 0: ephemeral
	Username  string
	Password  string
	UserAgent string // "siptooth/<version>"
	// TLSConfig nil means system roots and MinVersion TLS 1.2. Its ServerName
	// is always cleared: sipgo then sets it from the host of each
	// connection's destination, so a follow-up connection to a node Contact
	// verifies against that node's name.
	TLSConfig *tls.Config
	Logger    *slog.Logger
	// Resolver is used for every DNS lookup (sipgo's and ResolveNextHop's).
	// nil means net.DefaultResolver; tests inject their own.
	Resolver *net.Resolver
}

// UA is siptooth's SIP user agent. It is safe for concurrent use.
type UA struct {
	opts      Options
	transport string // lower case
	log       *slog.Logger
	resolver  *net.Resolver

	ua      *sipgo.UserAgent
	client  *sipgo.Client
	server  *sipgo.Server
	dialogs *sipgo.DialogClientCache

	// bgCtx is cancelled by Close; every background cleanup and keepalive
	// goroutine runs under it and is counted in loops, so Close returns only
	// when all of them are gone.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	// callKeepalive is the CRLF ping interval of an established TCP/TLS call.
	callKeepalive time.Duration
	// idleKeepalive is the registration's CRLF interval between calls.
	idleKeepalive time.Duration
	// regRetry is the wait before a failed REGISTER is retried.
	regRetry time.Duration
	// reinviteT1/T2/AckTimeout drive the 2xx retransmission of a re-INVITE
	// over UDP (RFC 3261 13.3.1.4); only tests change them.
	reinviteT1, reinviteT2, reinviteAckTimeout time.Duration
	// ioTimeout bounds ACK, CANCEL and cleanup BYE I/O (DNS, connect, handshake, write).
	ioTimeout time.Duration
	// regMinRefresh floors the REGISTER refresh interval.
	regMinRefresh time.Duration
	// reg is the active registration, if any (see register.go).
	reg atomic.Pointer[Registration]

	mu       sync.Mutex
	closed   bool
	cancels  []context.CancelFunc
	loops    sync.WaitGroup           // running keepalive and cleanup goroutines
	attempts map[string]*attempt      // INVITE attempts by Call-ID (see dial.go)
	timers   map[*time.Timer]struct{} // registry linger timers, stopped by Close
	calls    map[*sipgo.DialogClientSession]*call
	hops     map[string]netip.AddrPort // resolved next hops by "host:port" (see hop.go)
}

// NewUA builds the user agent. Nothing is sent and no socket is opened until
// the first request. When LocalPort > 0 the next hop is resolved once here to
// pick the local address toward it.
func NewUA(o Options) (*UA, error) {
	o.Transport = strings.ToLower(o.Transport)
	switch o.Transport {
	case "udp", "tcp", "tls":
	default:
		return nil, fmt.Errorf("sip: unsupported transport %q", o.Transport)
	}
	host, portStr, err := net.SplitHostPort(o.NextHop)
	if err != nil || host == "" {
		return nil, fmt.Errorf("sip: next hop %q must be host:port", o.NextHop)
	}
	if p, err := strconv.Atoi(portStr); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("sip: next hop %q has an invalid port", o.NextHop)
	}
	if o.LocalPort < 0 || o.LocalPort > 65535 {
		return nil, fmt.Errorf("sip: invalid local port %d", o.LocalPort)
	}
	if o.UserAgent == "" {
		o.UserAgent = "siptooth"
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	// sipgo logs raw packets when it cannot parse them: keep secrets out.
	sipLog := slog.New(newRedactHandler(log.Handler()))
	resolver := o.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.TLSConfig != nil {
		tlsConf = o.TLSConfig.Clone()
		if tlsConf.MinVersion == 0 {
			tlsConf.MinVersion = tls.VersionTLS12
		}
		tlsConf.ServerName = ""
	}

	// Spec 5.2: every TCP/TLS write is bounded. sipgo copies WriteTimeout
	// into each connection and sets a write deadline before every Write.
	transports := gosip.TransportsConfig{
		TCP: &gosip.TransportTCP{WriteTimeout: tcpWriteTimeout},
		TLS: &gosip.TransportTLS{TransportTCP: &gosip.TransportTCP{WriteTimeout: tcpWriteTimeout}},
	}

	sua, err := sipgo.NewUA(
		sipgo.WithUserAgent(o.UserAgent),
		sipgo.WithUserAgentDNSResolver(resolver),
		sipgo.WithUserAgenTLSConfig(tlsConf),
		sipgo.WithUserAgentTransportLayerOptions(
			gosip.WithTransportLayerLogger(sipLog),
			gosip.WithTransportLayerTransports(transports),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("sip: new user agent: %w", err)
	}

	u := &UA{
		opts: o, transport: o.Transport, log: log, resolver: resolver, ua: sua,
		callKeepalive: callKeepaliveEvery,
		idleKeepalive: idleKeepaliveEvery,
		ioTimeout:     cleanupTimeout,
		regMinRefresh: registerMinRefresh,
		regRetry:      registerRetryEvery,
		reinviteT1:    500 * time.Millisecond,
		reinviteT2:    4 * time.Second,

		reinviteAckTimeout: 32 * time.Second,
		hops:               make(map[string]netip.AddrPort),
		attempts:           make(map[string]*attempt),
		timers:             make(map[*time.Timer]struct{}),
		calls:              make(map[*sipgo.DialogClientSession]*call),
	}
	u.bgCtx, u.bgCancel = context.WithCancel(context.Background())

	copts := []sipgo.ClientOption{sipgo.WithClientLogger(sipLog)}
	if o.Transport == "udp" {
		copts = append(copts, sipgo.WithClientNAT())
	}
	if o.LocalPort > 0 || o.Transport == "udp" {
		// Bind the UDP socket (and, with LocalPort > 0, TCP/TLS dials) to the
		// local address the kernel picks toward the next hop, so Via and
		// Contact never carry 0.0.0.0. Port 0 means ephemeral.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		hop, err := u.ResolveNextHop(ctx)
		cancel()
		if err != nil {
			sua.Close()
			return nil, fmt.Errorf("sip: resolve next hop for local port: %w", err)
		}
		local, err := nat.LocalAddrToward(hop.Addr())
		if err != nil {
			sua.Close()
			return nil, fmt.Errorf("sip: local address toward %s: %w", hop.Addr(), err)
		}
		copts = append(copts, sipgo.WithClientConnectionAddr(netip.AddrPortFrom(local, uint16(o.LocalPort)).String()))
	}
	if u.client, err = sipgo.NewClient(sua, copts...); err != nil {
		sua.Close()
		return nil, fmt.Errorf("sip: new client: %w", err)
	}
	if u.server, err = sipgo.NewServer(sua, sipgo.WithServerLogger(sipLog)); err != nil {
		sua.Close()
		return nil, fmt.Errorf("sip: new server: %w", err)
	}
	// The cache needs a Contact at construction; callers put the real,
	// discovered one on every INVITE (spec 7.3), which takes precedence.
	u.dialogs = sipgo.NewDialogClientCache(u.client, gosip.ContactHeader{Address: gosip.Uri{
		Scheme: "sip", User: "siptooth", Host: "invalid",
	}})

	sua.TransportLayer().OnMessage(u.onMessage)
	u.installHandlers()
	return u, nil
}

// callKeepaliveEvery is the RFC 5626 CRLF ping interval on the connection of
// an established TCP/TLS call (spec 5.5). A variable only so tests can
// shorten it.
var callKeepaliveEvery = 25 * time.Second

// spawn runs f on a goroutine that Close waits for. It reports false (and
// does not run f) once the UA is closed.
func (u *UA) spawn(f func()) bool {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return false
	}
	u.loops.Add(1)
	u.mu.Unlock()
	go func() {
		defer u.loops.Done()
		f()
	}()
	return true
}

// Close stops keepalives, the transaction layer and every connection.
func (u *UA) Close() error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	u.bgCancel()
	cancels := u.cancels
	u.cancels = nil
	for t := range u.timers {
		t.Stop()
	}
	u.timers = nil
	u.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	err := u.ua.Close() // also unblocks a keepalive stuck in a write
	u.loops.Wait()
	return err
}

// Client is the sipgo client (WithClientNAT for UDP, bound to LocalPort when set).
func (u *UA) Client() *sipgo.Client { return u.client }

// Server is the sipgo server: register handlers (OnBye, OnAck, OnInvite,
// OnOptions, OnNoRoute) for far-end requests. It never listens; requests
// arrive on connections or the UDP flow siptooth opened.
func (u *UA) Server() *sipgo.Server { return u.server }

// Dialogs is the client dialog cache.
func (u *UA) Dialogs() *sipgo.DialogClientCache { return u.dialogs }

// UserAgent is the underlying sipgo user agent (transport layer access).
func (u *UA) UserAgent() *sipgo.UserAgent { return u.ua }

// Transport is "udp", "tcp" or "tls".
func (u *UA) Transport() string { return u.transport }

// NextHop is the configured next hop, "host:port".
func (u *UA) NextHop() string { return u.opts.NextHop }

// ResolveNextHop resolves the configured next hop through A/AAAA lookups (no
// NAPTR/SRV), preferring IPv4. A literal IP is returned as is. The answer is
// pinned: every later use (OPTIONS, REGISTER, INVITE, keepalive) sees the
// same address until a send to it fails (hop.go).
func (u *UA) ResolveNextHop(ctx context.Context) (netip.AddrPort, error) {
	return u.pinnedHop(ctx, u.opts.NextHop)
}

// preferIPv4 returns the first IPv4 address (4-in-6 unmapped), else the first address.
func preferIPv4(ips []netip.Addr) netip.Addr {
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap()
		}
	}
	if len(ips) > 0 {
		return ips[0].WithZone("")
	}
	return netip.Addr{}
}
