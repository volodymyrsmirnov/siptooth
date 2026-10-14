package placeholder

import (
	"reflect"
	"strings"
	"testing"
)

func TestNamesUnknown(t *testing.T) {
	if _, err := Names("sip:{foo}@h"); err == nil {
		t.Error("unknown placeholder must fail")
	}
	for _, bad := range []string{"{caller", "caller}", "{{caller}}", "{}"} {
		if _, err := Names(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	got, err := Names("{caller}-{number}-{caller}-{call_id}")
	if err != nil || !reflect.DeepEqual(got, []string{"caller", "number", "call_id"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestExpandEscaping(t *testing.T) {
	v := Values{Caller: "*31#123", Number: "+380", CallID: "id"}
	got, err := ExpandURI("sip:{caller}@h", v)
	if err != nil || got != "sip:*31%23123@h" {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = ExpandURI("sip:{number}@sip.example.com;transport=tls", v)
	if err != nil || got != "sip:+380@sip.example.com;transport=tls" {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = ExpandURI("sip:x@{call_id}.example.com;h={caller}", v)
	if err != nil || got != "sip:x@id.example.com;h=*31#123" {
		t.Errorf("host-side values must be verbatim: %q, %v", got, err)
	}
	got, err = ExpandURI("sip:{call_id}@h", Values{CallID: "a b%\"<>@:"})
	if err != nil || got != "sip:a%20b%25%22%3C%3E%40%3A@h" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestExpandURIInvalid(t *testing.T) {
	v := Values{Caller: "1"}
	for _, tmpl := range []string{"sips:{caller}@h", "tel:{caller}", "sip:{caller}@", "sip:", "{caller}@h", "sip:a@:5060"} {
		if _, err := ExpandURI(tmpl, v); err == nil {
			t.Errorf("%q must fail", tmpl)
		}
	}
	if _, err := ExpandURI("sip:a@{caller}", Values{Caller: "h h"}); err == nil {
		t.Error("whitespace in host must fail")
	}
	if got, err := ExpandURI("SIP:600@[::1]:5060", v); err != nil || got != "SIP:600@[::1]:5060" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := ExpandURI("sip:600@192.168.1.10", v); err != nil || got != "sip:600@192.168.1.10" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := ExpandURI("sip:host.example", v); err != nil || got != "sip:host.example" {
		t.Errorf("no-user URI: %q, %v", got, err)
	}
}

func TestExpandDisplayName(t *testing.T) {
	got := ExpandDisplayName("{caller} \\ x", Values{Caller: `Bob "the" \`})
	if got != `"Bob \"the\" \\ \\ x"` {
		t.Errorf("got %s", got)
	}
}

func TestExpandHeader(t *testing.T) {
	if got, err := ExpandHeader("{caller}/{call_id}", Values{Caller: "+1", CallID: "c"}); err != nil || got != "+1/c" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"a\r\nX: y", "a\nb", "a\rb", "a\x00b"} {
		if _, err := ExpandHeader("{caller}", Values{Caller: bad}); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if _, err := ExpandHeader("lit\r\n", Values{}); err == nil {
		t.Error("CRLF in template must fail")
	}
}

func TestExpandRaw(t *testing.T) {
	if got := Expand("{caller}|{number}|{call_id}|{x", Values{"a", "b", "c"}); got != "a|b|c|{x" {
		t.Errorf("got %q", got)
	}
}

func TestCleanCaller(t *testing.T) {
	cases := []struct {
		in       string
		typ      int
		want     string
		withheld bool
	}{
		{"380501234567", 145, "+380501234567", false},
		{"+380501234567", 145, "+380501234567", false},
		{" +38 (050) 1-2 ", 129, "+3805012", false},
		{"*31#123", 129, "*31#123", false},
		{"0501234567", 129, "0501234567", false},
		{"", 129, "", true},
		{"   ", 145, "", true},
		{"abc()", 145, "", true},
	}
	for _, c := range cases {
		got, w := CleanCaller(c.in, c.typ)
		if got != c.want || w != c.withheld {
			t.Errorf("CleanCaller(%q,%d) = %q,%v want %q,%v", c.in, c.typ, got, w, c.want, c.withheld)
		}
	}
	if got, _ := CleanCaller("1\r\n2", 129); strings.ContainsAny(got, "\r\n") {
		t.Error("CRLF survived")
	}
}
