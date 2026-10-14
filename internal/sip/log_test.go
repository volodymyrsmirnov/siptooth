package sip

import (
	"strings"
	"testing"
)

func TestRedactMessage(t *testing.T) {
	const key = "WVNfX19VX1NfRV9DX1JfRV9UX19fX0tfRV9ZX19fX18="
	in := "INVITE sip:600@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 192.0.2.1:5060;branch=z9hG4bK1\r\n" +
		"Authorization: Digest username=\"1234\", realm=\"r\", response=\"deadbeef\"\r\n" +
		"proxy-authorization: Digest username=\"1234\",\r\n" +
		"\tresponse=\"cafe\"\r\n" +
		"Call-ID: abc\r\n" +
		"\r\n" +
		"v=0\r\n" +
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + key + "|2^31\r\n" +
		"m=audio 4000 RTP/SAVP 0\r\n"

	got := RedactMessage(in)

	for _, secret := range []string{"deadbeef", "cafe", key, "username=\"1234\""} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted message still contains %q:\n%s", secret, got)
		}
	}
	for _, keep := range []string{
		"INVITE sip:600@example.com SIP/2.0",
		"Via: SIP/2.0/UDP 192.0.2.1:5060;branch=z9hG4bK1",
		"Call-ID: abc",
		"Authorization: <redacted>",
		"proxy-authorization: <redacted>",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<redacted>",
		"m=audio 4000 RTP/SAVP 0",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("redacted message lost %q:\n%s", keep, got)
		}
	}
	if strings.Count(got, "\r\n") != strings.Count(in, "\r\n")-1 {
		t.Errorf("folded continuation line should collapse into the redacted header:\n%q", got)
	}
}

func TestRedactMessageLeavesPlainMessagesAlone(t *testing.T) {
	in := "OPTIONS sip:x SIP/2.0\r\nCall-ID: 1\r\n\r\n"
	if got := RedactMessage(in); got != in {
		t.Errorf("RedactMessage changed a message with nothing secret: %q", got)
	}
}
