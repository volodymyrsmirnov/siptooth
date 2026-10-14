package hfp

import (
	"reflect"
	"testing"
)

func TestSplitLinesChunking(t *testing.T) {
	full := "\r\n+BRSF: 4095\r\n\r\nOK\r\n\r\nRING\r\n\r\n+CLIP: \"+15550100\",145\r\n"
	var got []string
	// feed one byte at a time
	var buf []byte
	for i := 0; i < len(full); i++ {
		buf = append(buf, full[i])
		var lines []string
		lines, buf = splitLines(buf)
		got = append(got, lines...)
	}
	want := []string{"+BRSF: 4095", "OK", "RING", "+CLIP: \"+15550100\",145"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("byte-wise: %q", got)
	}
	// feed everything at once
	lines, rest := splitLines([]byte(full))
	if !reflect.DeepEqual(lines, want) || len(rest) != 0 {
		t.Fatalf("coalesced: %q rest %q", lines, rest)
	}
	// partial trailing line stays in rest
	lines, rest = splitLines([]byte("\r\nOK\r\n\r\n+CIEV"))
	if !reflect.DeepEqual(lines, []string{"OK"}) || string(rest) != "+CIEV" {
		t.Fatalf("partial: %q rest %q", lines, rest)
	}
	// bare \n or stray \r are tolerated
	lines, _ = splitLines([]byte("OK\nRING\r\n"))
	if !reflect.DeepEqual(lines, []string{"OK", "RING"}) {
		t.Fatalf("lenient: %q", lines)
	}
}

func TestParseBRSF(t *testing.T) {
	if v, err := parseBRSF("+BRSF: 4095"); err != nil || v != 4095 {
		t.Fatal(v, err)
	}
	if _, err := parseBRSF("+BRSF:"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCINDList(t *testing.T) {
	line := `+CIND: ("service",(0,1)),("call",(0,1)),("callsetup",(0,3)),("battchg",(0,5)),("signal",(0,5)),("roam",(0,1)),("callheld",(0,2))`
	m, err := parseCINDList(line)
	if err != nil {
		t.Fatal(err)
	}
	if m["service"] != 1 || m["call"] != 2 || m["callsetup"] != 3 || m["callheld"] != 7 {
		t.Fatalf("%v", m)
	}
	// Android order variant: callsetup first
	m, _ = parseCINDList(`+CIND: ("callsetup",(0,3)),("call",(0,1))`)
	if m["callsetup"] != 1 || m["call"] != 2 {
		t.Fatalf("%v", m)
	}
}

func TestParseCINDValues(t *testing.T) {
	v, err := parseCINDValues("+CIND: 1,0,0,5,4,0,0")
	if err != nil || !reflect.DeepEqual(v, []int{1, 0, 0, 5, 4, 0, 0}) {
		t.Fatal(v, err)
	}
}

func TestParseCIEV(t *testing.T) {
	i, v, err := parseCIEV("+CIEV: 3,1")
	if err != nil || i != 3 || v != 1 {
		t.Fatal(i, v, err)
	}
	if _, _, err := parseCIEV("+CIEV: x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCLIP(t *testing.T) {
	n, typ, err := parseCLIP(`+CLIP: "+15550100",145`)
	if err != nil || n != "+15550100" || typ != 145 {
		t.Fatal(n, typ, err)
	}
	n, typ, err = parseCLIP(`+CLIP: "",128,,,"",0`) // withheld number
	if err != nil || n != "" || typ != 128 {
		t.Fatal(n, typ, err)
	}
	n, _, err = parseCLIP(`+CLIP: 5550100,129`) // unquoted
	if err != nil || n != "5550100" {
		t.Fatal(n, err)
	}
}

func TestIsFinal(t *testing.T) {
	for _, l := range []string{"OK", "ERROR", "+CME ERROR: 3", "NO CARRIER", "BUSY"} {
		if !isFinal(l) {
			t.Errorf("%q should be final", l)
		}
	}
	for _, l := range []string{"RING", "+CIEV: 1,1", "+BRSF: 1"} {
		if isFinal(l) {
			t.Errorf("%q should not be final", l)
		}
	}
}
