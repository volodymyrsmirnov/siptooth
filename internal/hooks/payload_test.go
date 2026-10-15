package hooks

import (
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 14, 9, 43, 31, 0, time.UTC)

func sample() CallInfo {
	caller := "+380501234567"
	ca := time.Date(2026, 10, 14, 9, 43, 35, 0, time.UTC)
	br := 12.5
	return CallInfo{ID: "20261014T094331Z-b5fa", Caller: &caller, Number: "+380441234567",
		Target: "sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls", SIPCallID: "abc@host", Codec: "PCMU", SRTP: true,
		RemoteMedia: "1.2.3.4:20000", AdvertisedMedia: "5.6.7.8:30000", NATMode: "stun", ConnectedAt: &ca,
		Outcome: "completed", HangupBy: "caller", SIPStatus: 200, SIPReason: "OK", RingingSeconds: 4.25, BridgedSeconds: &br,
		Media: &MediaStats{Sent: 10, Received: 9, Lost: 1, Late: 2, Duplicate: 3, Overflow: 4, DiscardedSilence: 5, SRTPFail: 6}}
}

func check(t *testing.T, got []byte, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want+"\n" {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

const ringingCallJSON = `{"id":"20261014T094331Z-b5fa","caller":"+380501234567","caller_withheld":false,"number":"+380441234567","target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls"`
const connectedFields = `"sip_call_id":"abc@host","codec":"PCMU","srtp":true,"remote_media":"1.2.3.4:20000","advertised_media":"5.6.7.8:30000","nat_mode":"stun","connected_at":"2026-10-14T09:43:35Z"`

func TestPayloadRinging(t *testing.T) {
	b, err := buildPayload("call.ringing", sample(), false, at)
	check(t, b, err, `{"schema_version":1,"event":"call.ringing","occurred_at":"2026-10-14T09:43:31Z","test":false,"call":`+ringingCallJSON+`}}`)
	if strings.Contains(string(b), "outcome") {
		t.Fatal("ringing must not have outcome")
	}
}

func TestPayloadRingingWithheld(t *testing.T) {
	c := sample()
	c.Caller, c.CallerWithheld = nil, true
	b, err := buildPayload("call.ringing", c, true, at)
	check(t, b, err, `{"schema_version":1,"event":"call.ringing","occurred_at":"2026-10-14T09:43:31Z","test":true,"call":{"id":"20261014T094331Z-b5fa","caller":null,"caller_withheld":true,"number":"+380441234567","target":"sip:+380441234567@sip.rtc.elevenlabs.io;transport=tls"}}`)
}

func TestPayloadConnected(t *testing.T) {
	b, err := buildPayload("call.connected", sample(), false, at)
	check(t, b, err, `{"schema_version":1,"event":"call.connected","occurred_at":"2026-10-14T09:43:31Z","test":false,"call":`+ringingCallJSON+`,`+connectedFields+`}}`)
}

func TestPayloadEnded(t *testing.T) {
	b, err := buildPayload("call.ended", sample(), false, at)
	check(t, b, err, `{"schema_version":1,"event":"call.ended","occurred_at":"2026-10-14T09:43:31Z","test":false,"call":`+ringingCallJSON+`,`+connectedFields+
		`,"outcome":"completed","hangup_by":"caller","sip_status":200,"sip_reason":"OK","error":null,"ringing_seconds":4.25,"bridged_seconds":12.5,`+
		`"media":{"sent":10,"received":9,"lost":1,"late":2,"duplicate":3,"overflow":4,"discarded_silence":5,"srtp_fail":6}}}`)
}

func TestPayloadEndedNulls(t *testing.T) {
	c := CallInfo{ID: "x", Number: "1", Target: "t", Outcome: "caller_abandoned", RingingSeconds: 2, Error: "boom"}
	b, err := buildPayload("call.ended", c, false, at)
	check(t, b, err, `{"schema_version":1,"event":"call.ended","occurred_at":"2026-10-14T09:43:31Z","test":false,"call":{"id":"x","caller":null,"caller_withheld":false,"number":"1","target":"t",`+
		`"sip_call_id":"","codec":"","srtp":false,"remote_media":"","advertised_media":"","nat_mode":"","connected_at":null,`+
		`"outcome":"caller_abandoned","hangup_by":null,"sip_status":null,"sip_reason":null,"error":"boom","ringing_seconds":2,"bridged_seconds":null,`+
		`"media":{"sent":0,"received":0,"lost":0,"late":0,"duplicate":0,"overflow":0,"discarded_silence":0,"srtp_fail":0}}}`)
}

func TestPayloadSystemError(t *testing.T) {
	b, err := buildErrorPayload("stun_failed", "no reply", false, at)
	check(t, b, err, `{"schema_version":1,"event":"system.error","occurred_at":"2026-10-14T09:43:31Z","test":false,"error":{"code":"stun_failed","detail":"no reply"}}`)
}

func TestPayloadUnknownEvent(t *testing.T) {
	if _, err := BuildPayload("system.error", sample(), false); err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildPayloadIsUTC(t *testing.T) {
	b, err := BuildPayload("call.ringing", sample(), true)
	if err != nil || !strings.Contains(string(b), `"occurred_at":"`) || !strings.Contains(string(b), `Z","test":true`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestPayloadSMS(t *testing.T) {
	from, name := "+380501234567", "Jane Doe"
	m := SMSInfo{ID: "20261014T094331Z-7c1e", From: &from, FromName: &name, Text: "Hello", Type: "SMS_GSM", Handle: "0400000000000012"}
	b, err := buildSMSPayload(m, false, at)
	check(t, b, err, `{"schema_version":1,"event":"sms.received","occurred_at":"2026-10-14T09:43:31Z","test":false,"sms":{"id":"20261014T094331Z-7c1e","from":"+380501234567","from_name":"Jane Doe","text":"Hello","type":"SMS_GSM","handle":"0400000000000012"}}`)
	m.From, m.FromName = nil, nil
	b, err = buildSMSPayload(m, true, at)
	check(t, b, err, `{"schema_version":1,"event":"sms.received","occurred_at":"2026-10-14T09:43:31Z","test":true,"sms":{"id":"20261014T094331Z-7c1e","from":null,"from_name":null,"text":"Hello","type":"SMS_GSM","handle":"0400000000000012"}}`)
}
