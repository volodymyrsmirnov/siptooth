package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
)

// ValidationError is one problem found by Validate.
type ValidationError struct {
	Field   string // YAML-style path, e.g. "sip.target"
	Problem string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Problem }

// MaxSocketPathLen is the longest control socket path accepted: sockaddr_un.sun_path holds
// 108 bytes on Linux and 104 on macOS, including the terminating NUL.
const MaxSocketPathLen = 100

var (
	macRe  = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
	idRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	userRe = regexp.MustCompile(`^[A-Za-z0-9\-_.!~*'()]+$`)
	// RFC 3261 token, used for header names.
	tokenRe = regexp.MustCompile("^[A-Za-z0-9\\-.!%*_+`'~]+$")
)

// forbiddenHeaders cannot be set through sip.headers (spec 5.2); Content-* is handled by prefix.
var forbiddenHeaders = map[string]bool{
	"call-id": true, "cseq": true, "via": true, "max-forwards": true, "contact": true,
	"from": true, "to": true, "route": true, "record-route": true, "allow": true,
	"supported": true, "require": true, "authorization": true, "proxy-authorization": true,
}

// compactHeaders maps RFC 3261 section 7.3.3 compact names to their long form.
var compactHeaders = map[string]string{
	"i": "call-id", "m": "contact", "e": "content-encoding", "l": "content-length",
	"c": "content-type", "f": "from", "s": "subject", "k": "supported", "t": "to",
	"v": "via", "o": "event", "r": "refer-to", "x": "session-expires", "u": "allow-events",
}

func forbiddenHeader(name string) bool {
	n := strings.ToLower(name)
	if long, ok := compactHeaders[n]; ok {
		n = long
	}
	return forbiddenHeaders[n] || strings.HasPrefix(n, "content-")
}

func hasCtl(s string) bool { return strings.ContainsAny(s, "\r\n\x00") }

// Validate checks required fields, ranges, enumerations and cross-field rules (spec
// section 9) and returns every problem found; nil means valid. password is the SIP digest
// password from LookupPassword (empty when unset).
func Validate(c Config, password string) []error {
	var errs []error
	add := func(field, format string, a ...any) {
		errs = append(errs, &ValidationError{Field: field, Problem: fmt.Sprintf(format, a...)})
	}
	inRange := func(field string, v, lo, hi int) {
		if v < lo || v > hi {
			add(field, "must be between %d and %d", lo, hi)
		}
	}
	noURIPassword := func(field, uri string) {
		rest := uri
		if len(rest) >= 4 && strings.EqualFold(rest[:4], "sip:") {
			rest = rest[4:]
		}
		if at := strings.IndexByte(rest, '@'); at >= 0 && strings.ContainsRune(rest[:at], ':') {
			add(field, "must not contain a password in the user part (sip:user:password@host); put credentials in %s", PasswordVar)
		}
	}
	noCtl := func(field, v string) bool {
		if hasCtl(v) {
			add(field, "must not contain CR, LF or NUL")
			return false
		}
		return true
	}

	// Bluetooth.
	if c.Bluetooth.Adapter == "" {
		add("bluetooth.adapter", "is required")
	}
	if c.Bluetooth.PhoneAddress == "" {
		add("bluetooth.phone_address", "is not set: run `siptooth pair` to pair the phone and fill it in")
	} else if !macRe.MatchString(c.Bluetooth.PhoneAddress) {
		add("bluetooth.phone_address", "must be a MAC address like AA:BB:CC:DD:EE:FF")
	}
	if v := c.Bluetooth.PowerCycleMinutes; v != 0 && (v < 10 || v > 1440) {
		add("bluetooth.power_cycle_minutes", "must be 0 (off) or between 10 and 1440")
	}

	// Phone.
	numberOK := noCtl("phone.number", c.Phone.Number)

	// SIP: placeholders. Every template is checked for known names; {number} needs phone.number.
	numberUsed := false
	checkTemplate := func(field, tmpl string) bool {
		if !noCtl(field, tmpl) {
			return false
		}
		names, err := placeholder.Names(tmpl)
		if err != nil {
			add(field, "%v", err)
			return false
		}
		for _, n := range names {
			if n == "number" {
				numberUsed = true
			}
		}
		return true
	}
	sampleNumber := c.Phone.Number
	if sampleNumber == "" || !numberOK {
		sampleNumber = "+10000000000"
	}
	sample := placeholder.Values{Caller: "+10000000000", Number: sampleNumber, CallID: "20261014T000000Z-abcd"}

	transport := c.SIP.Transport
	switch transport {
	case "udp", "tcp", "tls":
	default:
		add("sip.transport", "must be one of udp, tcp, tls")
	}

	if c.SIP.Target == "" {
		add("sip.target", "sip.target is required")
	} else if checkTemplate("sip.target", c.SIP.Target) {
		if len(c.SIP.Target) >= 5 && strings.EqualFold(c.SIP.Target[:5], "sips:") {
			add("sip.target", "sips: URIs are not supported; use sip: with ;transport=tls")
		} else if expanded, err := placeholder.ExpandURI(c.SIP.Target, sample); err != nil {
			add("sip.target", "%v", err)
		} else {
			for _, p := range placeholder.URIParams(expanded) {
				if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, "transport") &&
					transport != "" && !strings.EqualFold(v, transport) {
					add("sip.target", "transport parameter %q does not match sip.transport %q", v, transport)
				}
			}
		}
	}
	if c.SIP.Target != "" {
		noURIPassword("sip.target", c.SIP.Target)
	}
	if c.SIP.From != "" && checkTemplate("sip.from", c.SIP.From) {
		noURIPassword("sip.from", c.SIP.From)
		if _, err := placeholder.ExpandURI(c.SIP.From, sample); err != nil {
			add("sip.from", "%v", err)
		}
	}
	checkTemplate("sip.display_name", c.SIP.DisplayName)

	if c.SIP.AnonymousUser == "" || !noCtl("sip.anonymous_user", c.SIP.AnonymousUser) {
		if c.SIP.AnonymousUser == "" {
			add("sip.anonymous_user", "is required")
		}
	} else if !userRe.MatchString(c.SIP.AnonymousUser) {
		add("sip.anonymous_user", "may contain only letters, digits and -_.!~*'()")
	}

	if c.SIP.Proxy != "" && noCtl("sip.proxy", c.SIP.Proxy) {
		if _, port, err := net.SplitHostPort(c.SIP.Proxy); err != nil {
			add("sip.proxy", "must be host:port: %v", err)
		} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			add("sip.proxy", "port must be between 1 and 65535")
		}
	}
	if c.SIP.Registrar != "" && noCtl("sip.registrar", c.SIP.Registrar) {
		if err := checkHostPort(c.SIP.Registrar); err != nil {
			add("sip.registrar", "must be host or host:port without a user part: %v", err)
		}
	}
	inRange("sip.local_port", c.SIP.LocalPort, 0, 65535)
	if c.SIP.LocalPort != 0 && transport != "udp" {
		add("sip.local_port", "can be set only with sip.transport udp (TCP and TLS use an ephemeral port)")
	}
	noCtl("sip.username", c.SIP.Username)
	if c.SIP.Username != "" && password == "" {
		add("sip.username", "is set but %s is empty: put it in the environment or ~/.siptooth/env", PasswordVar)
	}
	if c.SIP.Register && c.SIP.Username == "" {
		add("sip.register", "sip.register requires sip.username")
	}
	inRange("sip.register_expires_seconds", c.SIP.RegisterExpiresSeconds, 60, 3600)
	inRange("sip.dial_timeout_seconds", c.SIP.DialTimeoutSeconds, 5, 120)
	if c.SIP.SRTP && transport != "tls" {
		add("sip.srtp", "sip.srtp requires sip.transport tls")
	}
	headerNames := make([]string, 0, len(c.SIP.Headers))
	for name := range c.SIP.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		value := c.SIP.Headers[name]
		field := "sip.headers[" + strconv.Quote(name) + "]"
		if !tokenRe.MatchString(name) {
			add("sip.headers", "header name %q is not a valid token", name)
			continue
		}
		if forbiddenHeader(name) {
			add("sip.headers", "header %q is not allowed (siptooth sets it itself)", name)
			continue
		}
		checkTemplate(field, value)
	}
	if numberUsed && c.Phone.Number == "" {
		add("phone.number", "is required because a placeholder {number} is used")
	}

	// Media.
	if len(c.Media.Codecs) == 0 {
		add("media.codecs", "needs at least one codec")
	}
	seenCodec := map[g711.Codec]bool{}
	for _, name := range c.Media.Codecs {
		codec, err := g711.ParseCodec(name)
		if err != nil {
			add("media.codecs", "%v", err)
		} else if seenCodec[codec] {
			add("media.codecs", "codec %q is listed twice", name)
		}
		seenCodec[codec] = true
	}
	inRange("media.jitter_ms", c.Media.JitterMs, 20, 500)
	inRange("media.silence_rms", c.Media.SilenceRMS, 0, 5000)
	inRange("media.min_pause_ms", c.Media.MinPauseMs, 0, 2000)
	inRange("media.echo_threshold_db", c.Media.EchoThresholdDB, -30, 20)
	inRange("media.echo_window_ms", c.Media.EchoWindowMs, 100, 2000)

	// NAT.
	switch c.NAT.Mode {
	case "auto", "local", "stun", "public_ip":
	default:
		add("nat.mode", "must be one of auto, local, stun, public_ip")
	}
	if c.NAT.PublicIP != "" {
		if a, err := netip.ParseAddr(c.NAT.PublicIP); err != nil || !a.Is4() {
			add("nat.public_ip", "must be an IPv4 address")
		}
	} else if c.NAT.Mode == "public_ip" {
		add("nat.public_ip", "is required when nat.mode is public_ip")
	}
	for _, s := range c.NAT.StunServers {
		if _, port, err := net.SplitHostPort(s); err != nil || port == "" || hasCtl(s) {
			add("nat.stun_servers", "%q must be host:port", s)
		}
	}
	if c.NAT.RTPBind != "" {
		if ip, err := netip.ParseAddr(c.NAT.RTPBind); err != nil {
			add("nat.rtp_bind", "must be an IP address")
		} else if !ip.Is4() {
			add("nat.rtp_bind", "must be an IPv4 address (the media path is IPv4 only)")
		} else if ip.IsUnspecified() {
			add("nat.rtp_bind", "must be a specific address, not 0.0.0.0 or :: (it is advertised to the far end)")
		}
	}
	lo, hi := c.NAT.RTPPortMin, c.NAT.RTPPortMax
	if lo < 1024 || lo > 65534 {
		add("nat.rtp_port_min", "must be between 1024 and 65534")
	}
	if hi < 1024 || hi > 65534 {
		add("nat.rtp_port_max", "must be between 1024 and 65534")
	}
	if lo%2 != 0 {
		add("nat.rtp_port_min", "must be even")
	}
	if hi%2 != 0 {
		add("nat.rtp_port_max", "must be even")
	}
	if lo >= hi {
		add("nat.rtp_port_min", "must be less than nat.rtp_port_max")
	}

	// Calls.
	inRange("calls.caller_id_wait_ms", c.Calls.CallerIDWaitMs, 0, 3000)
	inRange("calls.sco_settle_ms", c.Calls.SCOSettleMs, 0, 5000)
	inRange("calls.media_timeout_seconds", c.Calls.MediaTimeoutSeconds, 5, 300)
	if v := c.Calls.MaxDurationSeconds; v != 0 && (v < 60 || v > 86400) {
		add("calls.max_duration_seconds", "must be 0 (no limit) or between 60 and 86400")
	}

	// Control.
	if c.Control.Socket == "" {
		add("control.socket", "is required")
	} else if len(c.Control.Socket) > MaxSocketPathLen {
		add("control.socket", "path %q is too long (%d bytes, Unix sockets allow at most %d); set control.socket to a shorter absolute path such as /tmp/siptooth.sock",
			c.Control.Socket, len(c.Control.Socket), MaxSocketPathLen)
	}

	// Hooks.
	seen := map[string]bool{}
	for i, h := range c.Hooks {
		f := fmt.Sprintf("hooks[%d]", i)
		if !idRe.MatchString(h.ID) {
			add(f+".id", "must be a lowercase identifier")
		} else if seen[h.ID] {
			add(f+".id", "%q is duplicated", h.ID)
		}
		seen[h.ID] = true
		if len(h.Events) == 0 {
			add(f+".events", "must not be empty")
		}
		for _, e := range h.Events {
			if !contains(KnownEvents, e) {
				add(f+".events", "unknown event %q (known: %s)", e, strings.Join(KnownEvents, ", "))
			}
		}
		if strings.TrimSpace(h.Command) == "" {
			add(f+".command", "is required")
		}
		inRange(f+".timeout_seconds", h.TimeoutSeconds, 1, 300)
	}
	return errs
}

var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9\-_.]*[A-Za-z0-9])?$`)

// checkHostPort accepts host or host:port; an IPv6 literal needs brackets.
func checkHostPort(v string) error {
	host, port := v, ""
	if strings.HasPrefix(v, "[") {
		end := strings.IndexByte(v, ']')
		if end < 0 {
			return fmt.Errorf("unterminated IPv6 literal")
		}
		host = v[1:end]
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() {
			return fmt.Errorf("%q is not an IPv6 address", host)
		}
		rest := v[end+1:]
		if rest == "" {
			return nil
		}
		if rest[0] != ':' {
			return fmt.Errorf("unexpected %q after the IPv6 literal", rest)
		}
		port = rest[1:]
	} else if i := strings.IndexByte(v, ':'); i >= 0 {
		if strings.Count(v, ":") > 1 {
			return fmt.Errorf("an IPv6 address needs brackets")
		}
		host, port = v[:i], v[i+1:]
	}
	if port != "" || strings.HasSuffix(v, ":") {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
	}
	if !strings.HasPrefix(v, "[") && !hostnameRe.MatchString(host) {
		return fmt.Errorf("%q is not a host name or IPv4 address", host)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
