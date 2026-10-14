// Package placeholder expands the {caller}, {number} and {call_id} template
// placeholders with escaping appropriate to where the value lands (spec 5.1),
// and cleans +CLIP caller numbers (spec 4.7).
package placeholder

import (
	"fmt"
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

// checkSIPURI is a minimal sanity check: scheme "sip:", non-empty host, no
// whitespace or control characters.
func checkSIPURI(u string) error {
	if len(u) < 4 || !strings.EqualFold(u[:4], "sip:") {
		return fmt.Errorf("not a sip: URI")
	}
	for i := 0; i < len(u); i++ {
		if c := u[i]; c <= ' ' || c == 0x7F || c == '<' || c == '>' || c == '"' {
			return fmt.Errorf("invalid character %q in URI", c)
		}
	}
	rest := u[4:]
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	if i := strings.IndexAny(rest, ";?"); i >= 0 {
		rest = rest[:i]
	}
	host := rest
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 2 {
			return fmt.Errorf("empty or malformed IPv6 host")
		}
		return nil
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return fmt.Errorf("empty host")
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
