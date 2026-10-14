// Package placeholder expands the {caller}, {number} and {call_id} template
// placeholders with escaping appropriate to where the value lands (spec 5.1),
// and cleans +CLIP caller numbers (spec 4.7).
package placeholder

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Values are the substitution values. Caller is already cleaned, or the
// anonymous user when the caller is withheld.
type Values struct{ Caller, Number, CallID string }

// Context says where a substituted value lands.
type Context int

const (
	URIUser Context = iota
	URIHost
	DisplayName
	HeaderValue
)

var allowed = map[string]bool{"caller": true, "number": true, "call_id": true}

type segment struct {
	text        string
	placeholder bool
}

// parse splits tmpl into literal and placeholder segments, strictly.
func parse(tmpl string) ([]segment, error) {
	var segs []segment
	var lit strings.Builder
	for i := 0; i < len(tmpl); i++ {
		switch c := tmpl[i]; c {
		case '{':
			end := strings.IndexByte(tmpl[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unbalanced '{' at offset %d in %q", i, tmpl)
			}
			name := tmpl[i+1 : i+1+end]
			if !allowed[name] {
				return nil, fmt.Errorf("unknown placeholder {%s} in %q (allowed: {caller}, {number}, {call_id})", name, tmpl)
			}
			if lit.Len() > 0 {
				segs = append(segs, segment{text: lit.String()})
				lit.Reset()
			}
			segs = append(segs, segment{text: name, placeholder: true})
			i += end + 1
		case '}':
			return nil, fmt.Errorf("unbalanced '}' at offset %d in %q", i, tmpl)
		default:
			lit.WriteByte(c)
		}
	}
	if lit.Len() > 0 {
		segs = append(segs, segment{text: lit.String()})
	}
	return segs, nil
}

// Names returns the distinct placeholders used, in first-use order. It errors
// on unknown placeholders and unbalanced braces.
func Names(tmpl string) ([]string, error) {
	segs, err := parse(tmpl)
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, s := range segs {
		if s.placeholder && !seen[s.text] {
			seen[s.text] = true
			names = append(names, s.text)
		}
	}
	return names, nil
}

func (v Values) get(name string) string {
	switch name {
	case "caller":
		return v.Caller
	case "number":
		return v.Number
	}
	return v.CallID
}

// Expand substitutes without any escaping. Placeholder-like text that does not
// parse is left as is; validate templates with Names first.
func Expand(tmpl string, v Values) string {
	segs, err := parse(tmpl)
	if err != nil {
		for _, n := range []string{"caller", "number", "call_id"} {
			tmpl = strings.ReplaceAll(tmpl, "{"+n+"}", v.get(n))
		}
		return tmpl
	}
	return render(segs, func(string, int) Context { return HeaderValue }, v)
}

func render(segs []segment, ctxOf func(string, int) Context, v Values) string {
	var b strings.Builder
	for i, s := range segs {
		if !s.placeholder {
			b.WriteString(s.text)
			continue
		}
		b.WriteString(escape(ctxOf(s.text, i), v.get(s.text)))
	}
	return b.String()
}

// escape adjusts a substituted value for its context. DisplayName escaping
// covers only the inner quoted-string content; callers add the quotes.
func escape(ctx Context, val string) string {
	switch ctx {
	case URIUser:
		return escapeUser(val)
	case DisplayName:
		return escapeQuoted(val)
	}
	return val
}

// RFC 3261 user = 1*(unreserved / escaped / user-unreserved),
// unreserved = alphanum / mark, user-unreserved = "&=+$,;?/".
func userAllowed(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-_.!~*'()&=+$,;?/", c) >= 0
}

func escapeUser(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if userAllowed(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

func escapeQuoted(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// ExpandURI expands a sip: URI template. Values landing in the user part (before
// the first '@') are percent-encoded; values elsewhere are inserted verbatim.
// The result must be a sip: URI with a non-empty host and no whitespace or
// control characters.
func ExpandURI(tmpl string, v Values) (string, error) {
	segs, err := parse(tmpl)
	if err != nil {
		return "", err
	}
	// Index of the segment containing the first literal '@'; placeholders before it are user part.
	atSeg := -1
	for i, s := range segs {
		if !s.placeholder && strings.Contains(s.text, "@") {
			atSeg = i
			break
		}
	}
	out := render(segs, func(_ string, i int) Context {
		if atSeg >= 0 && i < atSeg {
			return URIUser
		}
		return URIHost
	}, v)
	if err := checkSIPURI(out); err != nil {
		return "", fmt.Errorf("expanding %q: %w", tmpl, err)
	}
	return out, nil
}

// splitURI splits a sip: URI (scheme already verified) into hostport and the
// parameter list. The user part ends at the first '@' that precedes any '?'
// (a user part cannot contain an unescaped '?'); ';' and '?' inside it are not
// URI parameters.
func splitURI(u string) (hostport string, params []string) {
	rest := u[4:]
	q := strings.IndexByte(rest, '?')
	if at := strings.IndexByte(rest, '@'); at >= 0 && (q < 0 || at < q) {
		rest = rest[at+1:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, ";")
	return parts[0], parts[1:]
}

// URIParams returns the URI parameters (the ";name=value" items after the host
// part, without headers) of a sip: URI. Text in the user part or the headers
// never counts. It returns nil for anything that does not start with "sip:".
func URIParams(u string) []string {
	if len(u) < 4 || !strings.EqualFold(u[:4], "sip:") {
		return nil
	}
	_, params := splitURI(u)
	return params
}

var hostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// checkSIPURI checks the scheme "sip:", the absence of whitespace and control
// characters, and a well-formed host (hostname, IPv4 or [IPv6]) with an
// optional port in 1..65535.
func checkSIPURI(u string) error {
	if len(u) < 4 || !strings.EqualFold(u[:4], "sip:") {
		return fmt.Errorf("not a sip: URI")
	}
	for i := 0; i < len(u); i++ {
		if c := u[i]; c <= ' ' || c == 0x7F || c == '<' || c == '>' || c == '"' {
			return fmt.Errorf("invalid character %q in URI", c)
		}
	}
	hostport, _ := splitURI(u)
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 2 {
			return fmt.Errorf("empty or malformed IPv6 host")
		}
		if a, err := netip.ParseAddr(hostport[1:end]); err != nil || !a.Is6() {
			return fmt.Errorf("invalid IPv6 host %q", hostport[:end+1])
		}
		rest := hostport[end+1:]
		if rest != "" {
			if rest[0] != ':' {
				return fmt.Errorf("unexpected text after IPv6 host")
			}
			port = rest[1:]
			if port == "" {
				return fmt.Errorf("empty port")
			}
		}
	} else {
		if i := strings.IndexByte(hostport, ':'); i >= 0 {
			host, port = hostport[:i], hostport[i+1:]
			if port == "" {
				return fmt.Errorf("empty port")
			}
		}
		if host == "" {
			return fmt.Errorf("empty host")
		}
		if err := checkHostname(host); err != nil {
			return err
		}
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strings.TrimLeft(port, "0123456789") != "" {
			return fmt.Errorf("invalid port %q", port)
		}
	}
	return nil
}

func checkHostname(host string) error {
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return fmt.Errorf("invalid host %q", host)
		}
		return nil
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for _, l := range labels {
		if len(l) > 63 || !hostLabelRe.MatchString(l) {
			return fmt.Errorf("invalid host %q", host)
		}
	}
	// A dotted-numeric name that is not an IPv4 address is malformed.
	if last := labels[len(labels)-1]; strings.TrimLeft(last, "0123456789") == "" {
		return fmt.Errorf("invalid host %q", host)
	}
	return nil
}

// ExpandDisplayName expands into a quoted-string, escaping '"' and '\'.
func ExpandDisplayName(tmpl string, v Values) string {
	segs, err := parse(tmpl)
	if err != nil {
		return `"` + escapeQuoted(Expand(tmpl, v)) + `"`
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, s := range segs {
		if s.placeholder {
			b.WriteString(escapeQuoted(v.get(s.text)))
		} else {
			b.WriteString(escapeQuoted(s.text))
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ExpandHeader expands a header value and rejects CR, LF and NUL in the result.
func ExpandHeader(tmpl string, v Values) (string, error) {
	segs, err := parse(tmpl)
	if err != nil {
		return "", err
	}
	out := render(segs, func(string, int) Context { return HeaderValue }, v)
	if strings.ContainsAny(out, "\r\n\x00") {
		return "", fmt.Errorf("header value contains CR, LF or NUL")
	}
	return out, nil
}

// CleanCaller implements spec 4.7: trim, add '+' for type 145 numbers lacking
// one, and remove characters other than +0-9*#. Empty result means withheld.
func CleanCaller(number string, numberType int) (cleaned string, withheld bool) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(number) {
		if (r >= '0' && r <= '9') || r == '+' || r == '*' || r == '#' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || s == "+" {
		return "", true
	}
	if numberType == 145 && !strings.HasPrefix(s, "+") {
		s = "+" + s
	}
	return s, false
}
