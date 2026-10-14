package sip

import (
	"context"
	"log/slog"
	"regexp"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/sdp"
)

// authHeader matches an Authorization or Proxy-Authorization header including
// folded continuation lines. Group 1 is the header name as written.
var authHeader = regexp.MustCompile(`(?im)^((?:proxy-)?authorization)[ \t]*:[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`)

// RedactMessage returns a SIP message (or any part of one) that is safe to
// log (spec 12): Authorization and Proxy-Authorization values become
// "<redacted>" and SDES inline keys in a=crypto lines become
// "inline:<redacted>". Everything else is left untouched.
func RedactMessage(s string) string {
	s = authHeader.ReplaceAllString(s, "$1: <redacted>")
	return string(sdp.Redact([]byte(s)))
}

// LogMessage logs a SIP message at debug level, redacted. dir is "send" or
// "recv". It formats the message only when debug logging is enabled.
func LogMessage(l *slog.Logger, dir string, m gosip.Message) {
	if l == nil || !l.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	l.Debug("sip message", "dir", dir, "message", RedactMessage(m.String()))
}
