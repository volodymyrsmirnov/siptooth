// Package hooks runs owner-configured shell commands on call lifecycle events (spec section 10).
package hooks

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the payload schema_version.
const SchemaVersion = 1

// MediaStats are the RTP counters reported in call.ended.
type MediaStats struct {
	Sent             uint64 `json:"sent"`
	Received         uint64 `json:"received"`
	Lost             uint64 `json:"lost"`
	Late             uint64 `json:"late"`
	Duplicate        uint64 `json:"duplicate"`
	Overflow         uint64 `json:"overflow"`
	DiscardedSilence uint64 `json:"discarded_silence"`
	SRTPFail         uint64 `json:"srtp_fail"`
}

// CallInfo carries everything any call.* event may report; each event serializes its own subset.
// Empty strings and zero SIPStatus mean "none" and become JSON null where the spec allows it.
type CallInfo struct {
	ID, Number, Target string
	Caller             *string // nil when withheld
	CallerWithheld     bool

	SIPCallID, Codec, RemoteMedia, AdvertisedMedia, NATMode string
	SRTP                                                    bool
	ConnectedAt                                             *time.Time

	Outcome, HangupBy, SIPReason, Error string
	SIPStatus                           int
	RingingSeconds                      float64
	BridgedSeconds                      *float64
	Media                               *MediaStats
}

// clone deep-copies the pointer fields so a queued event is immune to later mutation.
func (c CallInfo) clone() CallInfo {
	if c.Caller != nil {
		v := *c.Caller
		c.Caller = &v
	}
	if c.ConnectedAt != nil {
		v := *c.ConnectedAt
		c.ConnectedAt = &v
	}
	if c.BridgedSeconds != nil {
		v := *c.BridgedSeconds
		c.BridgedSeconds = &v
	}
	if c.Media != nil {
		v := *c.Media
		c.Media = &v
	}
	return c
}

type errorView struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type envelope struct {
	SchemaVersion int        `json:"schema_version"`
	Event         string     `json:"event"`
	OccurredAt    time.Time  `json:"occurred_at"`
	Test          bool       `json:"test"`
	Call          any        `json:"call,omitempty"`
	Error         *errorView `json:"error,omitempty"`
}

type ringingCall struct {
	ID             string  `json:"id"`
	Caller         *string `json:"caller"`
	CallerWithheld bool    `json:"caller_withheld"`
	Number         string  `json:"number"`
	Target         string  `json:"target"`
}

type connectedCall struct {
	ringingCall
	SIPCallID       string     `json:"sip_call_id"`
	Codec           string     `json:"codec"`
	SRTP            bool       `json:"srtp"`
	RemoteMedia     string     `json:"remote_media"`
	AdvertisedMedia string     `json:"advertised_media"`
	NATMode         string     `json:"nat_mode"`
	ConnectedAt     *time.Time `json:"connected_at"`
}

type endedCall struct {
	connectedCall
	Outcome        string     `json:"outcome"`
	HangupBy       *string    `json:"hangup_by"`
	SIPStatus      *int       `json:"sip_status"`
	SIPReason      *string    `json:"sip_reason"`
	Error          *string    `json:"error"`
	RingingSeconds float64    `json:"ringing_seconds"`
	BridgedSeconds *float64   `json:"bridged_seconds"`
	Media          MediaStats `json:"media"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// BuildPayload renders the JSON payload (newline terminated) of a call.* event.
func BuildPayload(event string, c CallInfo, test bool) ([]byte, error) {
	return buildPayload(event, c, test, time.Now())
}

func buildPayload(event string, c CallInfo, test bool, at time.Time) ([]byte, error) {
	r := ringingCall{ID: c.ID, Caller: c.Caller, CallerWithheld: c.CallerWithheld, Number: c.Number, Target: c.Target}
	p := envelope{SchemaVersion: SchemaVersion, Event: event, OccurredAt: at.UTC().Truncate(time.Second), Test: test}
	switch event {
	case "call.ringing":
		p.Call = r
	case "call.connected", "call.ended":
		cc := connectedCall{ringingCall: r, SIPCallID: c.SIPCallID, Codec: c.Codec, SRTP: c.SRTP, RemoteMedia: c.RemoteMedia,
			AdvertisedMedia: c.AdvertisedMedia, NATMode: c.NATMode, ConnectedAt: c.ConnectedAt}
		if cc.ConnectedAt != nil {
			t := cc.ConnectedAt.UTC().Truncate(time.Second)
			cc.ConnectedAt = &t
		}
		if event == "call.connected" {
			p.Call = cc
			break
		}
		ec := endedCall{connectedCall: cc, Outcome: c.Outcome, HangupBy: nullable(c.HangupBy), SIPReason: nullable(c.SIPReason),
			Error: nullable(c.Error), RingingSeconds: c.RingingSeconds, BridgedSeconds: c.BridgedSeconds}
		if c.SIPStatus != 0 {
			s := c.SIPStatus
			ec.SIPStatus = &s
		}
		if c.Media != nil {
			ec.Media = *c.Media
		}
		p.Call = ec
	default:
		return nil, errUnknownEvent(event)
	}
	return marshalLine(p)
}

type errUnknownEvent string

func (e errUnknownEvent) Error() string { return "hooks: not a call event: " + string(e) }

// BuildErrorPayload renders the system.error payload.
func BuildErrorPayload(code, detail string, test bool) ([]byte, error) {
	return buildErrorPayload(code, detail, test, time.Now())
}

func buildErrorPayload(code, detail string, test bool, at time.Time) ([]byte, error) {
	return marshalLine(envelope{SchemaVersion: SchemaVersion, Event: "system.error", OccurredAt: at.UTC().Truncate(time.Second),
		Test: test, Error: &errorView{Code: code, Detail: detail}})
}

func marshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// SMSInfo describes a forwarded SMS (spec 17.9). From and FromName are nil when unknown.
type SMSInfo struct {
	ID, Handle, Type, Text string
	From, FromName         *string
}

type smsView struct {
	ID       string  `json:"id"`
	From     *string `json:"from"`
	FromName *string `json:"from_name"`
	Text     string  `json:"text"`
	Type     string  `json:"type"`
	Handle   string  `json:"handle"`
}

type smsEnvelope struct {
	SchemaVersion int       `json:"schema_version"`
	Event         string    `json:"event"`
	OccurredAt    time.Time `json:"occurred_at"`
	Test          bool      `json:"test"`
	SMS           smsView   `json:"sms"`
}

// BuildSMSPayload renders the sms.received payload (newline terminated).
func BuildSMSPayload(m SMSInfo, test bool) ([]byte, error) {
	return buildSMSPayload(m, test, time.Now())
}

func buildSMSPayload(m SMSInfo, test bool, at time.Time) ([]byte, error) {
	return marshalLine(smsEnvelope{SchemaVersion: SchemaVersion, Event: "sms.received", OccurredAt: at.UTC().Truncate(time.Second),
		Test: test, SMS: smsView{ID: m.ID, From: m.From, FromName: m.FromName, Text: m.Text, Type: m.Type, Handle: m.Handle}})
}

func (m SMSInfo) clone() SMSInfo {
	if m.From != nil {
		v := *m.From
		m.From = &v
	}
	if m.FromName != nil {
		v := *m.FromName
		m.FromName = &v
	}
	return m
}
