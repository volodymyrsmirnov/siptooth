package sms

import (
	"io"
	"mime/quotedprintable"
	"strings"
	"unicode/utf8"
)

// vcardLines turns raw lines into logical ones: continuation lines (leading
// space or tab) are unfolded, and a quoted-printable line ending in "=" (a
// vCard 2.1 soft break) continues on the next line (spec 17.6).
func vcardLines(raw []string) []string {
	var out []string
	for i := 0; i < len(raw); i++ {
		line := raw[i]
		for {
			if i+1 < len(raw) && raw[i+1] != "" && (raw[i+1][0] == ' ' || raw[i+1][0] == '\t') {
				line += raw[i+1][1:]
				i++
				continue
			}
			if i+1 < len(raw) && strings.HasSuffix(line, "=") && vcardParams(line).qp {
				line = line[:len(line)-1] + raw[i+1]
				i++
				continue
			}
			break
		}
		out = append(out, line)
	}
	return out
}

type vcardHead struct {
	name    string
	qp      bool
	charset string // "" when absent
	value   string
}

// vcardParams splits "NAME;PARAM=V;BARE:value". Bare parameters are the vCard
// 2.1 form (QUOTED-PRINTABLE, UTF-8, CELL, ...).
func vcardParams(line string) vcardHead {
	head, value, _ := strings.Cut(line, ":")
	parts := strings.Split(head, ";")
	h := vcardHead{name: strings.ToUpper(strings.TrimSpace(parts[0])), value: value}
	if i := strings.LastIndexByte(h.name, '.'); i >= 0 {
		h.name = h.name[i+1:] // group prefix such as item1.TEL
	}
	for _, p := range parts[1:] {
		k, v, hasV := strings.Cut(p, "=")
		k = strings.ToUpper(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch {
		case !hasV && k == "QUOTED-PRINTABLE":
			h.qp = true
		case k == "ENCODING" && strings.EqualFold(v, "QUOTED-PRINTABLE"):
			h.qp = true
		case k == "CHARSET":
			h.charset = strings.ToUpper(v)
		}
	}
	return h
}

// decodeText decodes one raw value component: quoted-printable, UTF-8
// validity, then 3.0 escapes. ok is false when the value must be dropped.
func decodeText(s string, h vcardHead) (string, bool) {
	if h.charset != "" && h.charset != "UTF-8" {
		return "", false
	}
	if h.qp {
		d, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
		if err != nil {
			return "", false
		}
		s = string(d)
	}
	if !utf8.ValidString(s) {
		return "", false
	}
	return unescapeText(s), true
}

func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\', ',', ';':
				b.WriteByte(s[i+1])
				i++
				continue
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// splitUnescaped splits at ';' not preceded by a backslash.
func splitUnescaped(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == ';' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// parseVCard extracts the sender number and display name from the lines
// between BEGIN:VCARD and END:VCARD (exclusive). name is empty when absent or
// equal to the number.
func parseVCard(raw []string) (from, name string) {
	var fn, n string
	for _, line := range vcardLines(raw) {
		h := vcardParams(line)
		switch h.name {
		case "TEL":
			if from != "" {
				continue
			}
			if v, ok := decodeText(h.value, h); ok {
				from = strings.TrimSpace(v)
			}
		case "FN":
			if fn != "" {
				continue
			}
			if v, ok := decodeText(h.value, h); ok {
				fn = strings.TrimSpace(v)
			}
		case "N":
			if n != "" {
				continue
			}
			comps := splitUnescaped(h.value)
			dec := func(i int) string {
				if i >= len(comps) {
					return ""
				}
				v, ok := decodeText(comps[i], h)
				if !ok {
					return ""
				}
				return strings.TrimSpace(v)
			}
			var parts []string
			for _, c := range []string{dec(1), dec(0)} {
				if c != "" {
					parts = append(parts, c)
				}
			}
			n = strings.Join(parts, " ")
		}
	}
	name = fn
	if name == "" {
		name = n
	}
	if name == from {
		name = ""
	}
	return from, name
}
