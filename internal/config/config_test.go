package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// valid returns a configuration that passes Validate with password "pw".
func valid() Config {
	c := Default()
	c.Bluetooth.PhoneAddress = "AA:BB:CC:DD:EE:FF"
	c.Phone.Number = "+380441234567"
	c.SIP.Target = "sip:{number}@sip.rtc.elevenlabs.io;transport=tls"
	return c
}

func problems(c Config, pw string) string {
	var parts []string
	for _, e := range Validate(c, pw) {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "\n")
}

func TestValidBase(t *testing.T) {
	if p := problems(valid(), ""); p != "" {
		t.Fatal(p)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		pw     string
		want   string
	}{
		{"local_port with tls", func(c *Config) { c.SIP.LocalPort = 5070 }, "", "sip.local_port"},
		{"target password", func(c *Config) { c.SIP.Target = "sip:600:secret@h.example;transport=tls" }, "", "sip.target: must not contain a password"},
		{"from password", func(c *Config) { c.SIP.From = "sip:me:secret@h.example" }, "", "sip.from: must not contain a password"},
		{"registrar password", func(c *Config) { c.SIP.Registrar = "sip:u:p@reg.example" }, "", "sip.registrar"},
		{"registrar user part", func(c *Config) { c.SIP.Registrar = "u@reg.example" }, "", "sip.registrar"},
		{"registrar bad port", func(c *Config) { c.SIP.Registrar = "reg.example:99999" }, "", "sip.registrar"},
		{"registrar bare ipv6", func(c *Config) { c.SIP.Registrar = "2001:db8::1" }, "", "sip.registrar"},
		{"rtp_bind ipv6", func(c *Config) { c.NAT.RTPBind = "2001:db8::1" }, "", "nat.rtp_bind: must be an IPv4"},
		{"missing target", func(c *Config) { c.SIP.Target = "" }, "", "sip.target is required"},
		{"sips target", func(c *Config) { c.SIP.Target = "sips:host.example" }, "", "sips:"},
		{"not a sip uri", func(c *Config) { c.SIP.Target = "http://x" }, "", "sip.target"},
		{"transport mismatch", func(c *Config) { c.SIP.Transport = "udp" }, "", "transport"},
		{"bad transport", func(c *Config) { c.SIP.Transport = "ws"; c.SIP.Target = "sip:h" }, "", "sip.transport: must be one of"},
		{"srtp without tls", func(c *Config) { c.SIP.Transport = "udp"; c.SIP.Target = "sip:h;transport=udp"; c.SIP.SRTP = true }, "", "sip.srtp requires"},
		{"number without phone.number", func(c *Config) { c.Phone.Number = "" }, "", "phone.number"},
		{"number in header without phone.number", func(c *Config) {
			c.Phone.Number = ""
			c.SIP.Target = "sip:x@h;transport=tls"
			c.SIP.Headers = map[string]string{"X-N": "{number}"}
		}, "", "phone.number"},
		{"unknown placeholder in target", func(c *Config) { c.SIP.Target = "sip:{nope}@h;transport=tls" }, "", "unknown placeholder"},
		{"unknown placeholder in display", func(c *Config) { c.SIP.DisplayName = "{x}" }, "", "sip.display_name"},
		{"unknown placeholder in from", func(c *Config) { c.SIP.From = "sip:{x}@h" }, "", "sip.from"},
		{"unknown placeholder in header", func(c *Config) { c.SIP.Headers = map[string]string{"X-A": "{x}"} }, "", "sip.headers"},
		{"register without username", func(c *Config) { c.SIP.Register = true }, "", "sip.register requires sip.username"},
		{"username without password", func(c *Config) { c.SIP.Username = "u" }, "", SIPPasswordVarForTest},
		{"username with password ok", func(c *Config) { c.SIP.Username = "u" }, "pw", ""},
		{"forbidden header call-id", func(c *Config) { c.SIP.Headers = map[string]string{"call-id": "x"} }, "", "not allowed"},
		{"forbidden header Via", func(c *Config) { c.SIP.Headers = map[string]string{"Via": "x"} }, "", "not allowed"},
		{"forbidden header Authorization", func(c *Config) { c.SIP.Headers = map[string]string{"AUTHORIZATION": "x"} }, "", "not allowed"},
		{"forbidden header Content-Type", func(c *Config) { c.SIP.Headers = map[string]string{"content-type": "x"} }, "", "not allowed"},
		{"X-CALL-ID allowed", func(c *Config) {
			c.SIP.Headers = map[string]string{"X-CALL-ID": "{call_id}", "X-CALLER-ID": "{caller}"}
		}, "", ""},
		{"CR in header value", func(c *Config) { c.SIP.Headers = map[string]string{"X-A": "a\rb"} }, "", "CR, LF or NUL"},
		{"LF in header value", func(c *Config) { c.SIP.Headers = map[string]string{"X-A": "a\nb"} }, "", "CR, LF or NUL"},
		{"NUL in header name", func(c *Config) { c.SIP.Headers = map[string]string{"X-A\x00": "b"} }, "", "sip.headers"},
		{"bad header name", func(c *Config) { c.SIP.Headers = map[string]string{"X A": "b"} }, "", "sip.headers"},
		{"CR in number", func(c *Config) { c.Phone.Number = "+1\r\n" }, "", "phone.number"},
		{"LF in display name", func(c *Config) { c.SIP.DisplayName = "a\nb" }, "", "sip.display_name"},
		{"NUL in from", func(c *Config) { c.SIP.From = "sip:a\x00@h" }, "", "sip.from"},
		{"CR in target", func(c *Config) { c.SIP.Target = "sip:a@h\r\n" }, "", "sip.target"},
		{"LF in username", func(c *Config) { c.SIP.Username = "u\nx" }, "pw", "sip.username"},
		{"CR in anonymous", func(c *Config) { c.SIP.AnonymousUser = "a\rb" }, "", "sip.anonymous_user"},
		{"anonymous bad char", func(c *Config) { c.SIP.AnonymousUser = "a@b" }, "", "sip.anonymous_user"},
		{"anonymous space", func(c *Config) { c.SIP.AnonymousUser = "a b" }, "", "sip.anonymous_user"},
		{"anonymous empty", func(c *Config) { c.SIP.AnonymousUser = "" }, "", "sip.anonymous_user"},
		{"anonymous marks ok", func(c *Config) { c.SIP.AnonymousUser = "a-b_c.d!~*'()" }, "", ""},
		{"public_ip mode without ip", func(c *Config) { c.NAT.Mode = "public_ip" }, "", "nat.public_ip"},
		{"public_ip mode with ip", func(c *Config) { c.NAT.Mode = "public_ip"; c.NAT.PublicIP = "203.0.113.5" }, "", ""},
		{"bad ipv4", func(c *Config) { c.NAT.PublicIP = "999.1.1.1" }, "", "nat.public_ip"},
		{"ipv6 public ip", func(c *Config) { c.NAT.PublicIP = "2001:db8::1" }, "", "nat.public_ip"},
		{"bad nat mode", func(c *Config) { c.NAT.Mode = "x" }, "", "nat.mode"},
		{"unspecified rtp_bind v4", func(c *Config) { c.NAT.RTPBind = "0.0.0.0" }, "", "nat.rtp_bind"},
		{"unspecified rtp_bind v6", func(c *Config) { c.NAT.RTPBind = "::" }, "", "nat.rtp_bind"},
		{"bad rtp_bind", func(c *Config) { c.NAT.RTPBind = "nope" }, "", "nat.rtp_bind"},
		{"bad stun server", func(c *Config) { c.NAT.StunServers = []string{"nohostport"} }, "", "nat.stun_servers"},
		{"odd port min", func(c *Config) { c.NAT.RTPPortMin = 10001 }, "", "even"},
		{"odd port max", func(c *Config) { c.NAT.RTPPortMax = 20001 }, "", "even"},
		{"inverted port range", func(c *Config) { c.NAT.RTPPortMin = 20000; c.NAT.RTPPortMax = 10000 }, "", "less than"},
		{"port min below 1024", func(c *Config) { c.NAT.RTPPortMin = 1022 }, "", "1024"},
		{"port max above 65534", func(c *Config) { c.NAT.RTPPortMax = 65536 }, "", "65534"},
		{"hook unknown event", func(c *Config) {
			c.Hooks = []Hook{{ID: "a", Events: []string{"call.nope"}, Command: "x", TimeoutSeconds: 10}}
		}, "", "unknown event"},
		{"hook ok", func(c *Config) {
			c.Hooks = []Hook{{ID: "a", Events: []string{"call.ended", "system.error"}, Command: "x", TimeoutSeconds: 300}}
		}, "", ""},
		{"hook timeout too big", func(c *Config) {
			c.Hooks = []Hook{{ID: "a", Events: []string{"call.ended"}, Command: "x", TimeoutSeconds: 301}}
		}, "", "timeout_seconds"},
		{"hook timeout zero", func(c *Config) {
			c.Hooks = []Hook{{ID: "a", Events: []string{"call.ended"}, Command: "x"}}
		}, "", "timeout_seconds"},
		{"hook duplicate id", func(c *Config) {
			h := Hook{ID: "a", Events: []string{"call.ended"}, Command: "x", TimeoutSeconds: 10}
			c.Hooks = []Hook{h, h}
		}, "", "duplicated"},
		{"hook no command", func(c *Config) {
			c.Hooks = []Hook{{ID: "a", Events: []string{"call.ended"}, TimeoutSeconds: 10}}
		}, "", "command"},
		{"hook no events", func(c *Config) { c.Hooks = []Hook{{ID: "a", Command: "x", TimeoutSeconds: 10}} }, "", "events"},
		{"bad MAC", func(c *Config) { c.Bluetooth.PhoneAddress = "AA:BB" }, "", "bluetooth.phone_address"},
		{"missing MAC", func(c *Config) { c.Bluetooth.PhoneAddress = "" }, "", "siptooth pair"},
		{"missing adapter", func(c *Config) { c.Bluetooth.Adapter = "" }, "", "bluetooth.adapter"},
		{"no codecs", func(c *Config) { c.Media.Codecs = nil }, "", "media.codecs"},
		{"bad codec", func(c *Config) { c.Media.Codecs = []string{"OPUS"} }, "", "media.codecs"},
		{"missing socket", func(c *Config) { c.Control.Socket = "" }, "", "control.socket"},
		{"long socket", func(c *Config) { c.Control.Socket = "/" + strings.Repeat("a", 120) }, "", "too long"},
		{"proxy without port", func(c *Config) { c.SIP.Proxy = "proxy.example" }, "", "sip.proxy"},
		{"proxy ok", func(c *Config) { c.SIP.Proxy = "proxy.example:5061" }, "", ""},
		{"CR in proxy", func(c *Config) { c.SIP.Proxy = "p\r:5060" }, "", "sip.proxy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			got := problems(c, tc.pw)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("unexpected: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want substring %q, got %q", tc.want, got)
			}
		})
	}
}

// SIPPasswordVarForTest is the substring a missing-password error must mention.
const SIPPasswordVarForTest = "SIPTOOTH_SIP_PASSWORD"

func TestValidateRanges(t *testing.T) {
	type rng struct {
		name     string
		set      func(*Config, int)
		ok       []int
		bad      []int
		contains string
	}
	cases := []rng{
		{"power_cycle", func(c *Config, v int) { c.Bluetooth.PowerCycleMinutes = v }, []int{0, 10, 1440}, []int{-1, 9, 1441}, "power_cycle_minutes"},
		{"local_port", func(c *Config, v int) {
			c.SIP.Transport, c.SIP.Target, c.SIP.LocalPort = "udp", "sip:600@h.example;transport=udp", v
		}, []int{0, 1, 65535}, []int{-1, 65536}, "local_port"},
		{"register_expires", func(c *Config, v int) { c.SIP.RegisterExpiresSeconds = v }, []int{60, 3600}, []int{59, 3601}, "register_expires_seconds"},
		{"dial_timeout", func(c *Config, v int) { c.SIP.DialTimeoutSeconds = v }, []int{5, 120}, []int{4, 121}, "dial_timeout_seconds"},
		{"jitter", func(c *Config, v int) { c.Media.JitterMs = v }, []int{20, 500}, []int{19, 501}, "jitter_ms"},
		{"silence_rms", func(c *Config, v int) { c.Media.SilenceRMS = v }, []int{0, 5000}, []int{-1, 5001}, "silence_rms"},
		{"min_pause", func(c *Config, v int) { c.Media.MinPauseMs = v }, []int{0, 2000}, []int{-1, 2001}, "min_pause_ms"},
		{"caller_id_wait", func(c *Config, v int) { c.Calls.CallerIDWaitMs = v }, []int{0, 3000}, []int{-1, 3001}, "caller_id_wait_ms"},
		{"sco_settle", func(c *Config, v int) { c.Calls.SCOSettleMs = v }, []int{0, 5000}, []int{-1, 5001}, "sco_settle_ms"},
		{"media_timeout", func(c *Config, v int) { c.Calls.MediaTimeoutSeconds = v }, []int{5, 300}, []int{4, 301}, "media_timeout_seconds"},
		{"max_duration", func(c *Config, v int) { c.Calls.MaxDurationSeconds = v }, []int{0, 60, 86400}, []int{-1, 59, 86401}, "max_duration_seconds"},
	}
	for _, rc := range cases {
		for _, v := range rc.ok {
			c := valid()
			rc.set(&c, v)
			if p := problems(c, ""); p != "" {
				t.Errorf("%s=%d should be valid: %s", rc.name, v, p)
			}
		}
		for _, v := range rc.bad {
			c := valid()
			rc.set(&c, v)
			if p := problems(c, ""); !strings.Contains(p, rc.contains) {
				t.Errorf("%s=%d should be rejected, got %q", rc.name, v, p)
			}
		}
	}
}

func TestFromTemplateAndRegistrarHost(t *testing.T) {
	c := valid()
	if got := c.SIP.FromTemplate(); got != "sip:{caller}@sip.rtc.elevenlabs.io" {
		t.Fatal(got)
	}
	if got := c.SIP.RegistrarHost(); got != "sip.rtc.elevenlabs.io" {
		t.Fatal(got)
	}
	c.SIP.Target = "sip:600@192.168.1.10:5070"
	if got := c.SIP.FromTemplate(); got != "sip:{caller}@192.168.1.10" {
		t.Fatal(got)
	}
	c.SIP.Target = "sip:{number}@[2001:db8::1]:5070;transport=tcp"
	if got := c.SIP.RegistrarHost(); got != "[2001:db8::1]" {
		t.Fatal(got)
	}
	c.SIP.From = "sip:me@x.example"
	c.SIP.Registrar = "reg.example"
	if c.SIP.FromTemplate() != "sip:me@x.example" || c.SIP.RegistrarHost() != "reg.example" {
		t.Fatal("explicit values must win")
	}
}

func write(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadUnknownKey(t *testing.T) {
	p := write(t, t.TempDir(), "sip:\n  targett: x\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "targett") {
		t.Fatalf("unknown key must fail: %v", err)
	}
}

func TestLoadAppliesDefaultsAndResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "bluetooth:\n  phone_address: AA:BB:CC:DD:EE:FF\nhooks:\n  - id: a\n    events: [call.ended]\n    command: x\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(dir)
	if c.Control.Socket != filepath.Join(abs, "siptooth.sock") {
		t.Fatal(c.Control.Socket)
	}
	if c.SIP.Transport != "tls" || c.Media.JitterMs != 60 || c.Hooks[0].TimeoutSeconds != 10 {
		t.Fatalf("%+v", c)
	}
	if c.Bluetooth.Adapter != "hci0" {
		t.Fatal(c.Bluetooth.Adapter)
	}
}

func TestLoadKeepsAbsoluteSocket(t *testing.T) {
	p := write(t, t.TempDir(), "control:\n  socket: /tmp/s.sock\n")
	c, err := Load(p)
	if err != nil || c.Control.Socket != "/tmp/s.sock" {
		t.Fatalf("%v %q", err, c.Control.Socket)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLookupPasswordFeedsValidate(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "")
	t.Setenv(PasswordVar, "")
	os.WriteFile(filepath.Join(dir, "env"), []byte("SIPTOOTH_SIP_PASSWORD=file\n"), 0o600)
	pw, err := LookupPassword(p)
	if err != nil || pw != "file" {
		t.Fatal(pw, err)
	}
	t.Setenv(PasswordVar, "env")
	if pw, _ := LookupPassword(p); pw != "env" {
		t.Fatal("environment must beat the file")
	}
}

func equalConfig(a, b Config) bool {
	for _, c := range []*Config{&a, &b} {
		if len(c.SIP.Headers) == 0 {
			c.SIP.Headers = nil
		}
		if len(c.Hooks) == 0 {
			c.Hooks = nil
		}
	}
	return reflect.DeepEqual(a, b)
}

func TestCompactForbiddenHeaders(t *testing.T) {
	for _, name := range []string{"i", "m", "l", "c", "e", "f", "t", "v", "k", "I", "V"} {
		c := valid()
		c.SIP.Headers = map[string]string{name: "x"}
		if p := problems(c, ""); !strings.Contains(p, "not allowed") {
			t.Errorf("compact header %q must be rejected, got %q", name, p)
		}
	}
	for _, name := range []string{"s", "o", "r", "x", "u", "X-Foo"} {
		c := valid()
		c.SIP.Headers = map[string]string{name: "x"}
		if p := problems(c, ""); p != "" {
			t.Errorf("header %q must be allowed, got %q", name, p)
		}
	}
}

func TestHeaderErrorOrderDeterministic(t *testing.T) {
	c := valid()
	c.SIP.Headers = map[string]string{"Via": "x", "Call-ID": "x", "From": "x", "To": "x", "Route": "x"}
	first := problems(c, "")
	for i := 0; i < 20; i++ {
		if p := problems(c, ""); p != first {
			t.Fatalf("order changed:\n%s\n---\n%s", first, p)
		}
	}
}

func TestTargetTransportParamParsing(t *testing.T) {
	c := valid()
	c.SIP.Transport = "udp"
	// ';transport=tls' inside the user part is not a URI parameter.
	c.SIP.Target = "sip:a;transport=tls@host.example"
	if p := problems(c, ""); p != "" {
		t.Errorf("user-part text must not count as parameter: %q", p)
	}
	c.SIP.Target = "sip:a@host.example?x=;transport=tls"
	if p := problems(c, ""); p != "" {
		t.Errorf("header text must not count as parameter: %q", p)
	}
	c.SIP.Target = "sip:a@host.example;transport=tls"
	if p := problems(c, ""); !strings.Contains(p, "does not match") {
		t.Errorf("real mismatch must be reported: %q", p)
	}
}

func TestRegistrarAcceptsHostPort(t *testing.T) {
	for _, r := range []string{"reg.example", "reg.example:5061", "10.0.0.1:5060", "[2001:db8::1]", "[2001:db8::1]:5061"} {
		c := valid()
		c.SIP.Registrar = r
		if p := problems(c, ""); p != "" {
			t.Errorf("registrar %q rejected: %s", r, p)
		}
	}
	c := valid()
	c.SIP.Transport = "udp"
	c.SIP.Target = "sip:600@h.example;transport=udp"
	c.SIP.LocalPort = 5070
	if p := problems(c, ""); p != "" {
		t.Errorf("udp local_port rejected: %s", p)
	}
}

func TestRegistrationAOR(t *testing.T) {
	c := valid()
	c.SIP.Username = "alice"
	if got, want := c.SIP.RegistrationAOR(), "sip:alice@sip.rtc.elevenlabs.io"; got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
	c.SIP.Registrar = "reg.example:5061"
	if got, want := c.SIP.RegistrationAOR(), "sip:alice@reg.example:5061"; got != want {
		t.Errorf("registrar = %q, want %q", got, want)
	}
	c.SIP.From = "sip:+3120@example.org"
	if got := c.SIP.RegistrationAOR(); got != "sip:+3120@example.org" {
		t.Errorf("from = %q", got)
	}
	c.SIP.From = "sip:{caller}@example.org"
	if got := c.SIP.RegistrationAOR(); got != "sip:alice@reg.example:5061" {
		t.Errorf("placeholder from = %q, want the username form", got)
	}
}
