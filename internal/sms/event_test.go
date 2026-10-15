package sms

import (
	"errors"
	"strings"
	"testing"
)

func TestParseEventReport(t *testing.T) {
	const hdr = `<?xml version="1.0"?>` + "\r\n"
	tests := []struct {
		name string
		in   string
		want []Event
		fwd  []bool
		mms  []bool
	}{
		{"v1.0", hdr + `<MAP-event-report version="1.0"><event type="NewMessage" handle="0123" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/></MAP-event-report>`,
			[]Event{{"NewMessage", "0123", "TELECOM/MSG/INBOX", "SMS_GSM"}}, []bool{true}, []bool{false}},
		{"extra attributes", `<MAP-event-report version="1.1"><event type="NewMessage" handle="1" folder="telecom/msg/Inbox" msg_type="SMS_CDMA" subject="x" datetime="20240101T000000" sender_name="n"/></MAP-event-report>`,
			[]Event{{"NewMessage", "1", "telecom/msg/Inbox", "SMS_CDMA"}}, []bool{true}, []bool{false}},
		{"two events", `<MAP-event-report version="1.0"><event type="NewMessage" handle="1" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/><event type="NewMessage" handle="2" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/></MAP-event-report>`,
			[]Event{{"NewMessage", "1", "TELECOM/MSG/INBOX", "SMS_GSM"}, {"NewMessage", "2", "TELECOM/MSG/INBOX", "SMS_GSM"}}, []bool{true, true}, []bool{false, false}},
		{"mms", `<MAP-event-report version="1.0"><event type="NewMessage" handle="3" folder="TELECOM/MSG/INBOX" msg_type="MMS"/></MAP-event-report>`,
			[]Event{{"NewMessage", "3", "TELECOM/MSG/INBOX", "MMS"}}, []bool{false}, []bool{true}},
		{"sent folder", `<MAP-event-report version="1.0"><event type="NewMessage" handle="4" folder="TELECOM/MSG/SENT" msg_type="SMS_GSM"/></MAP-event-report>`,
			[]Event{{"NewMessage", "4", "TELECOM/MSG/SENT", "SMS_GSM"}}, []bool{false}, []bool{false}},
		{"other type", `<MAP-event-report version="1.0"><event type="MessageDeleted" handle="5" folder="TELECOM/MSG/INBOX" msg_type="SMS_GSM"/></MAP-event-report>`,
			[]Event{{"MessageDeleted", "5", "TELECOM/MSG/INBOX", "SMS_GSM"}}, []bool{false}, []bool{false}},
		{"no events", `<MAP-event-report version="1.0"></MAP-event-report>`, []Event{}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEventReport([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] || got[i].Forward() != tc.fwd[i] || got[i].IsMMS() != tc.mms[i] {
					t.Errorf("event %d: %+v fwd=%v mms=%v", i, got[i], got[i].Forward(), got[i].IsMMS())
				}
			}
		})
	}
}

func TestParseEventReportMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"truncated":  `<MAP-event-report version="1.0"><event type="NewMessage" handle="SECRETWORD"`,
		"mismatched": `<MAP-event-report><event handle="SECRETWORD"></evnt></MAP-event-report>`,
		"wrong root": `<other><event type="SECRETWORD"/></other>`,
		"empty":      ``,
		"not xml":    `SECRETWORD`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEventReport([]byte(in))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "SECRETWORD") {
				t.Fatal("error leaks input")
			}
		})
	}
}
