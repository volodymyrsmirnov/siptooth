package sms

import (
	"regexp"
	"strings"
)

// BMessage is the part of a MAP bMessage siptooth forwards.
type BMessage struct {
	Type, From, FromName, Text string
}

var endMsgEscape = regexp.MustCompile(`/(/*END:MSG)`)

// prop splits a bMessage line into an upper-cased property name (parameters
// dropped) and its value.
func prop(line string) (name, value string) {
	head, value, _ := strings.Cut(line, ":")
	head, _, _ = strings.Cut(head, ";")
	return strings.ToUpper(strings.TrimSpace(head)), value
}

func isBegin(line, what string) bool {
	n, v := prop(line)
	return n == "BEGIN" && strings.EqualFold(strings.TrimSpace(v), what)
}

func isEnd(line, what string) bool {
	n, v := prop(line)
	return n == "END" && strings.EqualFold(strings.TrimSpace(v), what)
}

func malformed(detail string) error { return &parseError{detail} }

// parseError wraps ErrMalformed with a constant detail (never input text).
type parseError struct{ detail string }

func (e *parseError) Error() string { return ErrMalformed.Error() + ": " + e.detail }
func (e *parseError) Unwrap() error { return ErrMalformed }

// ParseBMessage parses a GetMessage body (spec 17.6, "bMessage"). Errors wrap
// ErrMalformed or ErrUnsupported and never include message content.
func ParseBMessage(b []byte) (BMessage, error) {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || !isBegin(lines[i], "BMSG") {
		return BMessage{}, malformed("no BMSG")
	}
	i++

	var m BMessage
	var versionOK, haveVCard bool
	// BMSG properties and the originator vCard.
	for ; i < len(lines); i++ {
		line := lines[i]
		if isBegin(line, "BENV") {
			break
		}
		if isBegin(line, "VCARD") {
			j := i + 1
			for j < len(lines) && !isEnd(lines[j], "VCARD") {
				j++
			}
			if j >= len(lines) {
				return BMessage{}, malformed("unterminated vCard")
			}
			if !haveVCard {
				m.From, m.FromName = parseVCard(lines[i+1 : j])
				haveVCard = true
			}
			i = j
			continue
		}
		switch n, v := prop(line); n {
		case "VERSION":
			versionOK = strings.TrimSpace(v) == "1.0"
		case "TYPE":
			m.Type = strings.ToUpper(strings.TrimSpace(v))
		}
	}
	if !versionOK {
		return BMessage{}, malformed("version")
	}
	if m.Type != "SMS_GSM" && m.Type != "SMS_CDMA" {
		return BMessage{}, ErrUnsupported
	}
	if i >= len(lines) {
		return BMessage{}, malformed("no BENV")
	}
	i++ // BEGIN:BENV

	// Recipients are skipped; find BBODY.
	for ; i < len(lines) && !isBegin(lines[i], "BBODY"); i++ {
		if isBegin(lines[i], "VCARD") {
			for i < len(lines) && !isEnd(lines[i], "VCARD") {
				i++
			}
		}
	}
	if i >= len(lines) {
		return BMessage{}, malformed("no BBODY")
	}
	i++

	var charset string
	var text strings.Builder
	haveMsg, endBody := false, false
	for ; i < len(lines) && !endBody; i++ {
		line := lines[i]
		switch {
		case isEnd(line, "BBODY"):
			endBody = true
		case isBegin(line, "MSG"):
			j := i + 1
			for j < len(lines) && strings.TrimRight(lines[j], "\r") != "END:MSG" {
				j++
			}
			if j >= len(lines) {
				return BMessage{}, malformed("unterminated MSG")
			}
			text.WriteString(strings.Join(lines[i+1:j], "\n"))
			haveMsg = true
			i = j
		default:
			switch n, v := prop(line); n {
			case "CHARSET":
				charset = strings.TrimSpace(v)
			case "ENCODING":
				return BMessage{}, ErrUnsupported
			}
		}
	}
	if !endBody || !haveMsg {
		return BMessage{}, malformed("no message body")
	}
	if !strings.EqualFold(charset, "UTF-8") {
		return BMessage{}, ErrUnsupported
	}
	m.Text = endMsgEscape.ReplaceAllString(text.String(), "$1")

	// The outer structure must close.
	for ; i < len(lines); i++ {
		if isEnd(lines[i], "BMSG") {
			return m, nil
		}
	}
	return BMessage{}, malformed("no END:BMSG")
}
