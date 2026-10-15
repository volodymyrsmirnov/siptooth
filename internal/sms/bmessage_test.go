package sms

import (
	"errors"
	"strings"
	"testing"
)

// crlf builds a fixture with real CRLF line ends.
func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func bmsg(vcard, body string) string {
	return "BEGIN:BMSG\nVERSION:1.0\nSTATUS:UNREAD\nTYPE:SMS_GSM\nFOLDER:TELECOM/MSG/INBOX\n" + vcard +
		"BEGIN:BENV\nBEGIN:VCARD\nVERSION:2.1\nN:\nTEL:+1000\nEND:VCARD\nBEGIN:BBODY\nCHARSET:UTF-8\nLENGTH:27\nBEGIN:MSG\n" +
		body + "\nEND:MSG\nEND:BBODY\nEND:BENV\nEND:BMSG\n"
}

func TestParseBMessage(t *testing.T) {
	tests := []struct {
		name                string
		in                  string
		from, fromName, txt string
	}{
		{"vcard 2.1 quoted-printable cyrillic N with soft break",
			bmsg("BEGIN:VCARD\nVERSION:2.1\nN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:=D0=9E=D0=BB=\n=D1=8F;\nTEL:+380501234567\nEND:VCARD\n", "Hello"),
			"+380501234567", "Оля", "Hello"},
		{"vcard 3.0 escaped semicolon in N and comma in FN",
			bmsg("BEGIN:VCARD\nVERSION:3.0\nFN:Smith\\, John\nN:Sm\\;ith;Jo\\,hn;;;\nTEL:+1555\nEND:VCARD\n", "x"),
			"+1555", "Smith, John", "x"},
		{"N alone given and family",
			bmsg("BEGIN:VCARD\nVERSION:3.0\nN:Smith;John;;;\nTEL:+1555\nEND:VCARD\n", "x"),
			"+1555", "John Smith", "x"},
		{"N escaped semicolon",
			bmsg("BEGIN:VCARD\nVERSION:3.0\nN:Sm\\;ith;Jo\\,hn;;;\nTEL:+1555\nEND:VCARD\n", "x"),
			"+1555", "Jo,hn Sm;ith", "x"},
		{"empty FN falls to N",
			bmsg("BEGIN:VCARD\nVERSION:3.0\nFN:\nN:Doe;Jane\nTEL:+1\nEND:VCARD\n", "x"),
			"+1", "Jane Doe", "x"},
		{"name equal to number",
			bmsg("BEGIN:VCARD\nVERSION:2.1\nFN:+380501234567\nTEL:+380501234567\nEND:VCARD\n", "x"),
			"+380501234567", "", "x"},
		{"alphanumeric sender",
			bmsg("BEGIN:VCARD\nVERSION:2.1\nN:\nTEL:MyBank\nEND:VCARD\n", "x"),
			"MyBank", "", "x"},
		{"missing originator", bmsg("", "x"), "", "", "x"},
		{"TEL with parameters, first non-empty",
			bmsg("BEGIN:VCARD\nVERSION:2.1\nTEL;CELL:\nTEL;CELL;PREF:  +4412 \nTEL:+999\nEND:VCARD\n", "x"),
			"+4412", "", "x"},
		{"folded lines",
			bmsg("BEGIN:VCARD\nVERSION:3.0\nFN:Long\n  Name\nTEL:+38050\n 1234567\nEND:VCARD\n", "x"),
			"+380501234567", "Long Name", "x"},
		{"unsupported vcard charset dropped",
			bmsg("BEGIN:VCARD\nVERSION:2.1\nFN;CHARSET=ISO-8859-1:Caf\nN:Doe;Jane\nTEL:+1\nEND:VCARD\n", "x"),
			"+1", "Jane Doe", "x"},
		{"empty text", bmsg("", ""), "", "", ""},
		{"blank lines and trailing newline", bmsg("", "a\n\nb\n"), "", "", "a\n\nb\n"},
		{"END:MSG escaped", bmsg("", "a\n/END:MSG\nb"), "", "", "a\nEND:MSG\nb"},
		{"END:MSG double escaped", bmsg("", "a\n//END:MSG\nb"), "", "", "a\n/END:MSG\nb"},
		{"END:MSG inline", bmsg("", "see /END:MSG ok"), "", "", "see END:MSG ok"},
		{"slash without END:MSG kept", bmsg("", "a/b //c END:MS"), "", "", "a/b //c END:MS"},
	}
	for _, tc := range tests {
		for _, eol := range []string{"crlf", "lf"} {
			t.Run(tc.name+"/"+eol, func(t *testing.T) {
				in := tc.in
				if eol == "crlf" {
					in = crlf(in)
				}
				got, err := ParseBMessage([]byte(in))
				if err != nil {
					t.Fatal(err)
				}
				txt := tc.txt
				if got.From != tc.from || got.FromName != tc.fromName || got.Text != txt || got.Type != strings.ToUpper(got.Type) || got.Type != "SMS_GSM" {
					t.Errorf("got %+v", got)
				}
			})
		}
	}
}

func TestParseBMessageMultipleMSG(t *testing.T) {
	in := "BEGIN:BMSG\nVERSION:1.0\nTYPE:SMS_CDMA\nBEGIN:BENV\nBEGIN:BBODY\nCHARSET:UTF-8\nBEGIN:MSG\none\nEND:MSG\nBEGIN:MSG\ntwo\nEND:MSG\nEND:BBODY\nEND:BENV\nEND:BMSG\n"
	got, err := ParseBMessage([]byte(crlf(in)))
	if err != nil || got.Text != "onetwo" || got.Type != "SMS_CDMA" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseBMessageErrors(t *testing.T) {
	good := bmsg("", "SECRETWORD")
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"unsupported charset", strings.Replace(good, "CHARSET:UTF-8", "CHARSET:UCS-2", 1), ErrUnsupported},
		{"missing charset", strings.Replace(good, "CHARSET:UTF-8\n", "", 1), ErrUnsupported},
		{"encoding present", strings.Replace(good, "CHARSET:UTF-8\n", "CHARSET:UTF-8\nENCODING:8BIT\n", 1), ErrUnsupported},
		{"non-SMS type", strings.Replace(good, "TYPE:SMS_GSM", "TYPE:MMS", 1), ErrUnsupported},
		{"email type", strings.Replace(good, "TYPE:SMS_GSM", "TYPE:EMAIL", 1), ErrUnsupported},
		{"missing MSG", strings.Replace(strings.Replace(good, "BEGIN:MSG\n", "", 1), "END:MSG\n", "", 1), ErrMalformed},
		{"missing BBODY", "BEGIN:BMSG\nVERSION:1.0\nTYPE:SMS_GSM\nBEGIN:BENV\nSECRETWORD\nEND:BENV\nEND:BMSG\n", ErrMalformed},
		{"unterminated MSG", strings.Replace(good, "END:MSG\n", "", 1), ErrMalformed},
		{"wrong version", strings.Replace(good, "VERSION:1.0", "VERSION:2.0", 1), ErrMalformed},
		{"missing BMSG", "SECRETWORD\n", ErrMalformed},
		{"empty", "", ErrMalformed},
		{"no END:BMSG", strings.Replace(good, "END:BMSG\n", "", 1), ErrMalformed},
		{"no BENV", "BEGIN:BMSG\nVERSION:1.0\nTYPE:SMS_GSM\nSECRETWORD\n", ErrMalformed},
		{"unterminated vcard", "BEGIN:BMSG\nVERSION:1.0\nTYPE:SMS_GSM\nBEGIN:VCARD\nFN:SECRETWORD\n", ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBMessage([]byte(crlf(tc.in)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "SECRETWORD") {
				t.Fatal("error leaks input")
			}
		})
	}
}

func TestParseBMessageFixtureHasRealCRLF(t *testing.T) {
	if !strings.Contains(crlf("a\nb"), "\r\n") || strings.Contains(crlf("a\nb"), `\r`) {
		t.Fatal("fixture helper broken")
	}
}

func TestParseBMessageCaseInsensitiveProperties(t *testing.T) {
	in := "begin:bmsg\nversion:1.0\ntype:sms_gsm\nBegin:Vcard\nversion:2.1\ntel;cell:+1\nfn:Bob\nend:vcard\nbegin:benv\nbegin:bbody\ncharset:utf-8\nbegin:msg\nhi\nEND:MSG\nend:bbody\nend:benv\nend:bmsg\n"
	got, err := ParseBMessage([]byte(crlf(in)))
	if err != nil || got.From != "+1" || got.FromName != "Bob" || got.Text != "hi" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
