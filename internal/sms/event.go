package sms

import (
	"bytes"
	"encoding/xml"
	"errors"
	"strings"
)

// ErrUnsupported marks a message siptooth deliberately does not handle (a
// non-SMS bMessage type, a charset or encoding other than UTF-8). It never
// carries input text.
var ErrUnsupported = errors.New("sms: unsupported message")

// ErrMalformed marks input that does not parse. It never carries input text.
var ErrMalformed = errors.New("sms: malformed input")

// Event is one MAP event report entry (spec 17.6, "Event report"). Only the
// fields siptooth needs are kept; other attributes can carry a subject and are
// dropped on purpose.
type Event struct {
	Type, Handle, Folder, MsgType string
}

// Forward reports whether the event is a new SMS in the inbox.
func (e Event) Forward() bool {
	if e.Type != "NewMessage" {
		return false
	}
	if e.MsgType != "SMS_GSM" && e.MsgType != "SMS_CDMA" {
		return false
	}
	f := strings.TrimRight(e.Folder, "/")
	if i := strings.LastIndexByte(f, '/'); i >= 0 {
		f = f[i+1:]
	}
	return strings.EqualFold(f, "inbox")
}

// IsMMS reports whether the event announces a new MMS (logged at info so the
// owner sees why nothing arrived).
func (e Event) IsMMS() bool {
	return e.Type == "NewMessage" && e.MsgType == "MMS"
}

type xmlReport struct {
	XMLName xml.Name   `xml:"MAP-event-report"`
	Events  []xmlEvent `xml:"event"`
}

type xmlEvent struct {
	Type    string `xml:"type,attr"`
	Handle  string `xml:"handle,attr"`
	Folder  string `xml:"folder,attr"`
	MsgType string `xml:"msg_type,attr"`
}

// ParseEventReport parses an event report body. Events are returned in order;
// a report without events yields an empty slice. Errors wrap ErrMalformed and
// never include the input (encoding/xml errors can quote it).
func ParseEventReport(b []byte) ([]Event, error) {
	var r xmlReport
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&r); err != nil {
		return nil, ErrMalformed
	}
	evs := make([]Event, 0, len(r.Events))
	for _, e := range r.Events {
		evs = append(evs, Event{Type: e.Type, Handle: e.Handle, Folder: e.Folder, MsgType: e.MsgType})
	}
	return evs, nil
}
