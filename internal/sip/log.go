package sip

import (
	"context"
	"fmt"
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

// redactHandler wraps a slog.Handler so that nothing sipgo logs can carry
// credentials or SRTP keys: its parser logs the raw datagram when it cannot
// parse one, before any of our redaction runs. Every string (and error, and
// other value) in a record or in With attributes goes through RedactMessage.
type redactHandler struct{ inner slog.Handler }

func newRedactHandler(h slog.Handler) slog.Handler {
	if r, ok := h.(*redactHandler); ok {
		return r
	}
	return &redactHandler{inner: h}
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, RedactMessage(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(out)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactMessage(v.String()))
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, len(g))
		for i := range g {
			out[i] = redactAttr(g[i])
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, RedactMessage(x.Error()))
		case []byte:
			return slog.String(a.Key, RedactMessage(string(x)))
		case fmt.Stringer:
			return slog.String(a.Key, RedactMessage(x.String()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// defaultHandler forwards to slog.Default() at the time of each record, so
// the process-wide logger can be set later than sipgo's default logger.
type defaultHandler struct {
	wraps []func(slog.Handler) slog.Handler
}

func (d defaultHandler) target() slog.Handler {
	h := slog.Default().Handler()
	for _, w := range d.wraps {
		h = w(h)
	}
	return h
}

func (d defaultHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return d.target().Enabled(ctx, l)
}

func (d defaultHandler) Handle(ctx context.Context, r slog.Record) error {
	return d.target().Handle(ctx, r)
}

func (d defaultHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	w := append(append([]func(slog.Handler) slog.Handler(nil), d.wraps...),
		func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
	return defaultHandler{wraps: w}
}

func (d defaultHandler) WithGroup(name string) slog.Handler {
	w := append(append([]func(slog.Handler) slog.Handler(nil), d.wraps...),
		func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
	return defaultHandler{wraps: w}
}

// init makes sipgo's package-level logger (used by its TCP and TLS
// transports, whose logger cannot be set from outside) redact. It forwards to
// slog.Default() at the time of each record. It must run before any sipgo
// goroutine exists, hence init rather than NewUA.
func init() {
	gosip.SetDefaultLogger(slog.New(newRedactHandler(defaultHandler{})))
}
