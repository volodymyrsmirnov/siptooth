package sdp

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
)

var testKey = func() []byte {
	k := make([]byte, 30)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}()

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func offer(srtp bool) Offer {
	o := Offer{
		SessID: 1700000000, Version: 1,
		Addr:   netip.MustParseAddrPort("203.0.113.7:20000"),
		Codecs: []g711.Codec{g711.PCMU, g711.PCMA},
	}
	if srtp {
		o.LocalKey = testKey
	}
	return o
}

func crlf(lines ...string) []byte { return []byte(strings.Join(lines, "\r\n") + "\r\n") }

func answer(extra ...string) []byte {
	l := []string{"v=0", "o=pbx 1 1 IN IP4 198.51.100.9", "s=-", "c=IN IP4 198.51.100.9", "t=0 0", "m=audio 30000 RTP/AVP 0"}
	return crlf(append(l, extra...)...)
}

func srtpAnswer(cryptoLines ...string) []byte {
	l := []string{"v=0", "s=-", "c=IN IP4 198.51.100.9", "t=0 0", "m=audio 30000 RTP/SAVP 0"}
	return crlf(append(l, cryptoLines...)...)
}

func goodCrypto() string { return "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + b64(testKey) }

func TestOfferGolden(t *testing.T) {
	for _, tc := range []struct {
		file string
		srtp bool
	}{{"offer_plain.sdp", false}, {"offer_srtp.sdp", true}} {
		want, err := os.ReadFile("testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := offer(tc.srtp).Marshal(); !bytes.Equal(got, want) {
			t.Errorf("%s mismatch:\n%q\nwant\n%q", tc.file, got, want)
		}
	}
}

func TestOfferCodecOrder(t *testing.T) {
	o := offer(false)
	o.Codecs = []g711.Codec{g711.PCMA, g711.PCMU}
	if !strings.Contains(string(o.Marshal()), "RTP/AVP 8 0\r\n") {
		t.Errorf("order not followed: %q", o.Marshal())
	}
}

func TestNewKey(t *testing.T) {
	a, err := NewKey()
	if err != nil || len(a) != 30 {
		t.Fatalf("len=%d err=%v", len(a), err)
	}
	b, _ := NewKey()
	if bytes.Equal(a, b) {
		t.Error("keys repeat")
	}
}

func TestParseAnswer(t *testing.T) {
	short := make([]byte, 29)
	longKey := b64(testKey)
	cases := []struct {
		name string
		body []byte
		srtp bool
		ok   bool
		send g711.Codec
		recv []g711.Codec
		addr string
	}{
		{name: "pcmu only", body: answer(), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "198.51.100.9:30000"},
		{name: "both, pcma first", body: crlf("v=0", "c=IN IP4 1.2.3.4", "m=audio 4000 RTP/AVP 8 0"), ok: true, send: g711.PCMA, recv: []g711.Codec{g711.PCMU, g711.PCMA}, addr: "1.2.3.4:4000"},
		{name: "telephone-event ignored", body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/AVP 101 8", "a=rtpmap:101 telephone-event/8000"), ok: true, send: g711.PCMA, recv: []g711.Codec{g711.PCMA}, addr: "1.2.3.4:4000"},
		{name: "malformed first audio does not fall through", body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/AVP", "m=audio 5000 RTP/AVP 0")},
		{name: "bad port first audio does not fall through", body: crlf("c=IN IP4 1.2.3.4", "m=audio xx RTP/AVP 0", "m=audio 5000 RTP/AVP 0")},
		{name: "media c overrides", body: crlf("c=IN IP4 1.1.1.1", "m=audio 4000 RTP/AVP 0", "c=IN IP4 2.2.2.2"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "2.2.2.2:4000"},
		{name: "explicit sendrecv", body: answer("a=sendrecv"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "198.51.100.9:30000"},
		{name: "video first with own c and sendonly", body: crlf("c=IN IP4 1.2.3.4", "m=video 5000 RTP/AVP 96", "c=IN IP4 9.9.9.9", "a=sendonly", "m=audio 4000 RTP/AVP 0"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "1.2.3.4:4000"},
		{name: "video first, no session c", body: crlf("m=video 5000 RTP/AVP 96", "c=IN IP4 9.9.9.9", "m=audio 4000 RTP/AVP 0")},
		{name: "video after audio", body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/AVP 0", "m=video 5000 RTP/AVP 96", "c=IN IP4 9.9.9.9", "a=sendonly", goodCrypto()), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "1.2.3.4:4000"},
		{name: "media dir overrides session", body: crlf("c=IN IP4 1.2.3.4", "a=sendonly", "m=audio 4000 RTP/AVP 0", "a=sendrecv"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "1.2.3.4:4000"},
		{name: "session sendrecv", body: crlf("c=IN IP4 1.2.3.4", "a=sendrecv", "m=audio 4000 RTP/AVP 0"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "1.2.3.4:4000"},
		{name: "srtp crypto only in video", srtp: true, body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/SAVP 0", "m=video 5000 RTP/SAVP 96", goodCrypto())},
		{name: "port 0", body: crlf("c=IN IP4 1.2.3.4", "m=audio 0 RTP/AVP 0")},
		{name: "sendonly", body: answer("a=sendonly")},
		{name: "recvonly", body: answer("a=recvonly")},
		{name: "inactive", body: answer("a=inactive")},
		{name: "session-level sendonly", body: crlf("c=IN IP4 1.2.3.4", "a=sendonly", "m=audio 4000 RTP/AVP 0")},
		{name: "no connection", body: crlf("m=audio 4000 RTP/AVP 0")},
		{name: "ipv6", body: crlf("c=IN IP6 ::1", "m=audio 4000 RTP/AVP 0")},
		{name: "hostname", body: crlf("c=IN IP4 pbx.example.com", "m=audio 4000 RTP/AVP 0")},
		{name: "hold 0.0.0.0", body: crlf("c=IN IP4 0.0.0.0", "m=audio 4000 RTP/AVP 0")},
		{name: "payload 18 only", body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/AVP 18")},
		{name: "SAVP without our srtp", body: crlf("c=IN IP4 1.2.3.4", "m=audio 4000 RTP/SAVP 0", goodCrypto())},
		{name: "srtp offer answered AVP", srtp: true, body: answer()},
		{name: "srtp ok", srtp: true, body: srtpAnswer(goodCrypto()), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "198.51.100.9:30000"},
		{name: "srtp lifetime", srtp: true, body: srtpAnswer(goodCrypto() + "|2^31"), ok: true, send: g711.PCMU, recv: []g711.Codec{g711.PCMU}, addr: "198.51.100.9:30000"},
		{name: "srtp tag 2", srtp: true, body: srtpAnswer("a=crypto:2 AES_CM_128_HMAC_SHA1_80 inline:" + longKey)},
		{name: "srtp wrong suite", srtp: true, body: srtpAnswer("a=crypto:1 AES_CM_128_HMAC_SHA1_32 inline:" + longKey)},
		{name: "srtp 29-byte key", srtp: true, body: srtpAnswer("a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + b64(short))},
		{name: "srtp bad base64", srtp: true, body: srtpAnswer("a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:!!!!")},
		{name: "srtp MKI", srtp: true, body: srtpAnswer(goodCrypto() + "|2^20|1:4")},
		{name: "srtp MKI no lifetime", srtp: true, body: srtpAnswer(goodCrypto() + "|1:4")},
		{name: "srtp session params", srtp: true, body: srtpAnswer(goodCrypto() + " UNENCRYPTED_SRTP")},
		{name: "srtp no crypto", srtp: true, body: srtpAnswer()},
		{name: "srtp two cryptos", srtp: true, body: srtpAnswer(goodCrypto(), goodCrypto())},
		{name: "garbage", body: []byte("\x00\xff garbage\nm=audio\nm=audio x y z\n=\n")},
		{name: "empty", body: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseAnswer(tc.body, offer(tc.srtp))
			if !tc.ok {
				if !errors.Is(err, ErrUnacceptable) {
					t.Fatalf("err = %v, want ErrUnacceptable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.SendCodec != tc.send || !equalCodecs(m.Recv, tc.recv) || m.Addr.String() != tc.addr || m.SRTP != tc.srtp {
				t.Errorf("got %+v", m)
			}
			if tc.srtp != (m.RemoteKey != nil) || (tc.srtp && !bytes.Equal(m.RemoteKey, testKey)) {
				t.Errorf("RemoteKey = %x", m.RemoteKey)
			}
		})
	}
}

func equalCodecs(a, b []g711.Codec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseAnswerNeverPanics(t *testing.T) {
	for _, full := range [][]byte{srtpAnswer(goodCrypto() + "|2^31"), answer("a=sendrecv")} {
		for i := 0; i <= len(full); i++ {
			_, _ = ParseAnswer(full[:i], offer(true))
			_, _ = ParseAnswer(full[:i], offer(false))
			_, _, _ = ClassifyReoffer(full[:i], Media{SendCodec: g711.PCMU, SRTP: true, RemoteKey: testKey}, offer(true))
			_ = Redact(full[:i])
		}
	}
}

func TestClassifyReoffer(t *testing.T) {
	cur := Media{Addr: netip.MustParseAddrPort("198.51.100.9:30000"), SendCodec: g711.PCMU, Recv: []g711.Codec{g711.PCMU}}
	curS := cur
	curS.SRTP, curS.RemoteKey = true, testKey
	otherKey := append([]byte(nil), testKey...)
	otherKey[0] ^= 0xff

	cases := []struct {
		name string
		body []byte
		cur  Media
		want ReofferKind
		addr string
	}{
		{"same", answer(), cur, Refresh, "198.51.100.9:30000"},
		{"extra codec", crlf("c=IN IP4 198.51.100.9", "m=audio 30000 RTP/AVP 8 0"), cur, Refresh, "198.51.100.9:30000"},
		{"new port", crlf("c=IN IP4 198.51.100.9", "m=audio 30002 RTP/AVP 0"), cur, AddressChange, "198.51.100.9:30002"},
		{"new address", crlf("c=IN IP4 192.0.2.1", "m=audio 30000 RTP/AVP 0"), cur, AddressChange, "192.0.2.1:30000"},
		{"sendonly", answer("a=sendonly"), cur, Reject, ""},
		{"inactive", answer("a=inactive"), cur, Reject, ""},
		{"no our codec", crlf("c=IN IP4 198.51.100.9", "m=audio 30000 RTP/AVP 8"), cur, Reject, ""},
		{"profile change", srtpAnswer(goodCrypto()), cur, Reject, ""},
		{"srtp same key", srtpAnswer(goodCrypto()), curS, Refresh, "198.51.100.9:30000"},
		{"srtp lifetime same key", srtpAnswer(goodCrypto() + "|2^31"), curS, Refresh, "198.51.100.9:30000"},
		{"srtp key change", srtpAnswer("a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + b64(otherKey)), curS, Reject, ""},
		{"srtp to plain", answer(), curS, Reject, ""},
		{"port 0", crlf("c=IN IP4 198.51.100.9", "m=audio 0 RTP/AVP 0"), cur, Reject, ""},
		{"hold 0.0.0.0", crlf("c=IN IP4 0.0.0.0", "m=audio 30000 RTP/AVP 0"), cur, Reject, ""},
		{"video first with sendonly", crlf("c=IN IP4 198.51.100.9", "m=video 5000 RTP/AVP 96", "a=sendonly", "m=audio 30000 RTP/AVP 0"), cur, Refresh, "198.51.100.9:30000"},
		{"garbage", []byte("zzz"), cur, Reject, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, m, err := ClassifyReoffer(tc.body, tc.cur, offer(tc.cur.SRTP))
			if err != nil {
				t.Fatal(err)
			}
			if kind != tc.want {
				t.Fatalf("kind = %v, want %v", kind, tc.want)
			}
			if tc.addr != "" && m.Addr.String() != tc.addr {
				t.Errorf("addr = %v, want %s", m.Addr, tc.addr)
			}
			if kind == Reject && m.Addr != tc.cur.Addr {
				t.Errorf("Reject must return current media")
			}
		})
	}
	if _, _, err := ClassifyReoffer(nil, cur, offer(false)); err == nil {
		t.Error("empty body must error")
	}
	if _, _, err := ClassifyReoffer([]byte(" \r\n"), cur, offer(false)); err == nil {
		t.Error("blank body must error")
	}
}

func TestAnswerFor(t *testing.T) {
	o := offer(true)
	o.Version = 5
	m := Media{SendCodec: g711.PCMA, SRTP: true}
	got := string(AnswerFor(o, m))
	for _, want := range []string{
		"o=siptooth 1700000000 5 IN IP4 203.0.113.7\r\n",
		"c=IN IP4 203.0.113.7\r\n",
		"m=audio 20000 RTP/SAVP 8\r\n",
		"a=rtpmap:8 PCMA/8000\r\n",
		goodCrypto() + "\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "PCMU") {
		t.Error("answer must list only the send codec")
	}
	// The answer must itself be a valid answer to a plain offer round trip.
	p := AnswerFor(offer(false), Media{SendCodec: g711.PCMU})
	if strings.Contains(string(p), "crypto") || !strings.Contains(string(p), "RTP/AVP 0\r\n") {
		t.Errorf("plain answer wrong: %q", p)
	}
	if _, err := ParseAnswer(p, offer(false)); err != nil {
		t.Errorf("own answer rejected: %v", err)
	}
}

func TestRedact(t *testing.T) {
	for _, suffix := range []string{"", "|2^31", "|2^20|1:4", "|1:4"} {
		in := crlf("v=0", goodCrypto()+suffix, "a=sendrecv")
		out := string(Redact(in))
		if strings.Contains(out, b64(testKey)) || strings.Contains(out, "2^") || strings.Contains(out, "1:4") {
			t.Errorf("suffix %q leaked: %q", suffix, out)
		}
		if !strings.Contains(out, "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<redacted>\r\n") || !strings.Contains(out, "a=sendrecv") {
			t.Errorf("suffix %q: unexpected %q", suffix, out)
		}
	}
	if string(Redact(answer())) != string(answer()) {
		t.Error("body without keys must be unchanged")
	}
}
