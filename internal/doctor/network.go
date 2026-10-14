package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/buildinfo"
	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/nat"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
	"github.com/volodymyrsmirnov/siptooth/internal/sipaddr"
	"github.com/volodymyrsmirnov/siptooth/internal/stun"
)

// nextHop returns the next hop "host:port" (the daemon's rule, internal/sipaddr) and the
// trial-expanded target URI.
func nextHop(cfg config.Config) (hostPort, target string, err error) {
	target, err = placeholder.ExpandURI(cfg.SIP.Target, placeholder.Values{Caller: "doctor", Number: cfg.Phone.Number, CallID: "doctor"})
	if err != nil {
		return "", "", fmt.Errorf("sip.target: %w", err)
	}
	hostPort, err = sipaddr.NextHop(cfg)
	if err != nil {
		return "", "", err
	}
	return hostPort, target, nil
}

// networkChecks runs the observations against the next hop. Without a usable target they
// are skipped (the config check already fails).
func networkChecks(ctx context.Context, cfg config.Config, password string, opts Options) []Check {
	hop, target, err := nextHop(cfg)
	if err != nil {
		return []Check{skip("network", err.Error())}
	}
	host, _, _ := net.SplitHostPort(hop)
	var out []Check
	dns, addr := dnsCheck(ctx, host, opts.Resolver)
	out = append(out, dns)
	if !addr.IsValid() {
		return append(out, skip("sip_connect", "next hop does not resolve"), skip("local_address", "next hop does not resolve"),
			natCheck(ctx, cfg, opts), skip("sip_options", "next hop does not resolve"))
	}
	switch strings.ToLower(cfg.SIP.Transport) {
	case "tls":
		out = append(out, tlsCheck(ctx, hop, host, networkTimeout, nil))
	case "tcp":
		out = append(out, tcpCheck(ctx, hop, networkTimeout))
	default:
		out = append(out, skip("sip_connect", "udp: no connection to test"))
	}
	if local, err := nat.LocalAddrToward(addr); err != nil {
		out = append(out, warn("local_address", err.Error(), "check the default route"))
	} else {
		out = append(out, ok("local_address", fmt.Sprintf("%s toward %s (nat.mode %s)", local, addr, cfg.NAT.Mode)))
	}
	out = append(out, natCheck(ctx, cfg, opts))
	out = append(out, optionsCheck(ctx, cfg, password, hop, target, opts.Resolver))
	return out
}

func dnsCheck(ctx context.Context, host string, r *net.Resolver) (Check, netip.Addr) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return ok("dns", host+" is an IP literal"), ip.Unmap()
	}
	if r == nil {
		r = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		if err == nil {
			err = errors.New("no addresses")
		}
		return warn("dns", "cannot resolve "+host+": "+err.Error(), "check DNS and network access from the Pi"), netip.Addr{}
	}
	pick := ips[0]
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			pick = ip
			break
		}
	}
	return ok("dns", fmt.Sprintf("%s -> %s", host, pick.Unmap())), pick.Unmap()
}

func tcpCheck(ctx context.Context, hop string, timeout time.Duration) Check {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", hop)
	if err != nil {
		return warn("sip_connect", "TCP connect to "+hop+" failed: "+err.Error(), "check that the port is reachable from the Pi")
	}
	c.Close()
	return ok("sip_connect", "TCP connect to "+hop)
}

// tlsCheck dials hop with TLS using the system roots (roots non-nil in tests) and
// serverName as the name to verify. A certificate that does not verify is a failure, since
// calls would fail the same way; an unreachable server is only a warning.
func tlsCheck(ctx context.Context, hop, serverName string, timeout time.Duration, roots *x509.CertPool) Check {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	c, err := d.DialContext(ctx, "tcp", hop)
	if err != nil {
		var verr *tls.CertificateVerificationError
		if errors.As(err, &verr) {
			return fail("sip_connect", "TLS certificate of "+hop+" does not verify for "+serverName+": "+verr.Err.Error(),
				"fix the certificate on the server, or use sip.transport tcp/udp if the provider has no valid certificate")
		}
		return warn("sip_connect", "TLS connect to "+hop+" failed: "+err.Error(), "check that the port is reachable from the Pi")
	}
	state := c.(*tls.Conn).ConnectionState()
	c.Close()
	return ok("sip_connect", fmt.Sprintf("TLS to %s, certificate verified for %s (%s)", hop, serverName, tlsVersion(state.Version)))
}

func tlsVersion(v uint16) string {
	if s := tls.VersionName(v); s != "" {
		return s
	}
	return fmt.Sprintf("0x%04x", v)
}

// natCheck sends STUN Binding requests from ONE local socket to two STUN servers and
// compares the mapped ports (spec 7.4). Only observations are reported.
func natCheck(ctx context.Context, cfg config.Config, opts Options) Check {
	servers := opts.STUNServers
	if len(servers) == 0 {
		servers = cfg.NAT.StunServers
	}
	if len(servers) < 2 {
		return skip("nat_mapping", "fewer than two STUN servers configured (nat.stun_servers)")
	}
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return warn("nat_mapping", "cannot open a UDP socket: "+err.Error(), "")
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })() // cancel aborts a pending query
	return natObservation(conn, servers[:2], networkTimeout)
}

func natObservation(conn net.PacketConn, servers []string, timeout time.Duration) Check {
	var mapped [2]netip.AddrPort
	for i, s := range servers {
		ap, err := stun.Query(conn, s, timeout, 1)
		if err != nil {
			return warn("nat_mapping", "STUN query failed: "+err.Error(), "UDP to STUN servers may be blocked; set nat.mode to local or public_ip if the far end is reachable directly")
		}
		mapped[i] = ap
	}
	detail := fmt.Sprintf("%s -> %s, %s -> %s", servers[0], mapped[0], servers[1], mapped[1])
	if mapped[0].Port() == mapped[1].Port() {
		return ok("nat_mapping", "endpoint-independent mapping observed ("+detail+")")
	}
	return warn("nat_mapping", "address-dependent mapping: needs a latching far end ("+detail+")",
		"the far end must learn the RTP address from the first packets it receives (symmetric RTP)")
}

// optionsCheck sends an OPTIONS toward the target from a temporary user agent on an
// ephemeral local port and reports the received/rport the far end saw.
func optionsCheck(ctx context.Context, cfg config.Config, password, hop, target string, r *net.Resolver) Check {
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	ua, err := sip.NewUA(sip.Options{Transport: cfg.SIP.Transport, NextHop: hop, LocalPort: 0,
		Username: cfg.SIP.Username, Password: password, UserAgent: "siptooth/" + buildinfo.Version, Resolver: r})
	if err != nil {
		return warn("sip_options", err.Error(), "")
	}
	defer ua.Close()
	ap, found := ua.DiscoverContact(ctx, target)
	if !found {
		return warn("sip_options", "no OPTIONS response with received/rport from "+hop,
			"the far end may not answer OPTIONS or the network is unreachable; calls fall back to the media address IP")
	}
	return ok("sip_options", "far end saw this flow as "+ap.String()+" (received/rport)")
}
