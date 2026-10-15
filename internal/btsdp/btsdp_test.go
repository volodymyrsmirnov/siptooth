package btsdp

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestEncodeSearchAttrRequestGolden(t *testing.T) {
	got := EncodeSearchAttrRequest(0x0102, 0x1132, nil)
	want := []byte{0x06, 0x01, 0x02, 0x00, 0x0F,
		0x35, 0x03, 0x19, 0x11, 0x32,
		0x04, 0x00,
		0x35, 0x05, 0x0A, 0x00, 0x00, 0xFF, 0xFF,
		0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
	got = EncodeSearchAttrRequest(3, 0x1132, []byte{9, 8})
	want = append(append([]byte{}, want[:len(want)-1]...), 2, 9, 8)
	want[1], want[2], want[4] = 0, 3, 0x11
	if !bytes.Equal(got, want) {
		t.Fatalf("cont: got % x\nwant % x", got, want)
	}
}

func TestAddrHelpers(t *testing.T) {
	l2, err := L2Addr("AA:BB:CC:DD:EE:FF")
	if err != nil || l2 != [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF} {
		t.Fatalf("L2Addr = % x, %v", l2, err)
	}
	rf, err := RFCOMMAddr("AA:BB:CC:DD:EE:FF")
	if err != nil || rf != [6]byte{0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA} {
		t.Fatalf("RFCOMMAddr = % x, %v", rf, err)
	}
	for _, bad := range []string{"", "AA:BB", "zz:bb:cc:dd:ee:ff", "AA:BB:CC:DD:EE:FF:00:11"} {
		if _, err := L2Addr(bad); err == nil {
			t.Errorf("L2Addr(%q) accepted", bad)
		}
		if _, err := RFCOMMAddr(bad); err == nil {
			t.Errorf("RFCOMMAddr(%q) accepted", bad)
		}
	}
}

// resp builds a response PDU.
func resp(pdu byte, tid uint16, params []byte) []byte {
	return append([]byte{pdu, byte(tid >> 8), byte(tid), byte(len(params) >> 8), byte(len(params))}, params...)
}

func attrParams(attrs, cont []byte) []byte {
	p := []byte{byte(len(attrs) >> 8), byte(len(attrs))}
	p = append(p, attrs...)
	p = append(p, byte(len(cont)))
	return append(p, cont...)
}

func TestParseSearchAttrResponse(t *testing.T) {
	good := resp(7, 5, attrParams([]byte{1, 2, 3}, []byte{9}))
	a, c, err := ParseSearchAttrResponse(good, 5, 1024)
	if err != nil || !bytes.Equal(a, []byte{1, 2, 3}) || !bytes.Equal(c, []byte{9}) {
		t.Fatalf("good: %v %v %v", a, c, err)
	}
	bigCont := make([]byte, 17)
	tests := []struct {
		name string
		b    []byte
	}{
		{"short", []byte{7, 0}},
		{"wrong pdu", resp(0x05, 5, attrParams(nil, nil))},
		{"wrong tid", resp(7, 6, attrParams(nil, nil))},
		{"param length too big", append(append([]byte{}, good[:3]...), 0, 99, 0)},
		{"trailing bytes", append(append([]byte{}, good...), 0)},
		{"count over parameters", resp(7, 5, []byte{0, 50, 1, 2, 0})},
		{"count over maximum", resp(7, 5, attrParams(make([]byte, 1025), nil))},
		{"missing cont byte", resp(7, 5, []byte{0, 1, 1})},
		{"cont over 16", resp(7, 5, attrParams(nil, bigCont))},
		{"cont length lies", resp(7, 5, []byte{0, 0, 3, 1})},
		{"params too short", resp(7, 5, []byte{0, 0})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseSearchAttrResponse(tc.b, 5, 1024); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestSDPErrorPDU(t *testing.T) {
	_, _, err := ParseSearchAttrResponse(resp(1, 5, []byte{0, 3}), 5, 1024)
	var er *ErrorResponse
	if !errors.As(err, &er) || er.Code != 3 {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := ParseSearchAttrResponse(resp(1, 5, []byte{0}), 5, 1024); err == nil {
		t.Fatal("short error accepted")
	}
}

func TestDecodeElement(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		typ  int
		chk  func(Element) bool
	}{
		{"nil", []byte{0x00}, TypeNil, nil},
		{"uint8", []byte{0x08, 0x7F}, TypeUint, func(e Element) bool { return e.Uint == 0x7F }},
		{"uint16", []byte{0x09, 0x01, 0x02}, TypeUint, func(e Element) bool { return e.Uint == 0x102 }},
		{"uint32", []byte{0x0A, 0, 0, 1, 0}, TypeUint, func(e Element) bool { return e.Uint == 256 }},
		{"uint64", []byte{0x0B, 0, 0, 0, 0, 0, 0, 0, 5}, TypeUint, func(e Element) bool { return e.Uint == 5 }},
		{"uint128", append([]byte{0x0C}, make([]byte, 16)...), TypeUint, func(e Element) bool { return len(e.Raw) == 16 }},
		{"sint8", []byte{0x10, 0xFF}, TypeSint, nil},
		{"sint16", []byte{0x11, 0xFF, 0xFE}, TypeSint, nil},
		{"sint32", []byte{0x12, 1, 2, 3, 4}, TypeSint, nil},
		{"sint64", append([]byte{0x13}, make([]byte, 8)...), TypeSint, nil},
		{"sint128", append([]byte{0x14}, make([]byte, 16)...), TypeSint, nil},
		{"uint via size8", []byte{0x25, 3, 1, 2, 3}, -1, nil},
		{"uuid16", []byte{0x19, 0x11, 0x32}, TypeUUID, func(e Element) bool { return e.Uint == 0x1132 }},
		{"uuid32", []byte{0x1A, 0, 0, 0x11, 0x32}, TypeUUID, func(e Element) bool { return e.Uint == 0x1132 }},
		{"uuid128", append([]byte{0x1C}, make([]byte, 16)...), TypeUUID, nil},
		{"text8", []byte{0x25, 3, 'a', 'b', 0}, TypeText, func(e Element) bool { s, _ := e.Text(); return s == "ab" }},
		{"text16", []byte{0x26, 0, 2, 'h', 'i'}, TypeText, func(e Element) bool { s, _ := e.Text(); return s == "hi" }},
		{"text32", []byte{0x27, 0, 0, 0, 1, 'x'}, TypeText, nil},
		{"bool", []byte{0x28, 1}, TypeBool, func(e Element) bool { return e.Bool }},
		{"seq8 nested", []byte{0x35, 0x05, 0x35, 0x03, 0x08, 1, 0x00}, TypeSeq, func(e Element) bool {
			return len(e.Children) == 1 && len(e.Children[0].Children) == 2
		}},
		{"seq16", []byte{0x36, 0, 2, 0x08, 1}, TypeSeq, nil},
		{"seq32", []byte{0x37, 0, 0, 0, 2, 0x08, 1}, TypeSeq, nil},
		{"alt", []byte{0x3D, 2, 0x08, 1}, TypeAlt, nil},
		{"url", []byte{0x45, 2, 'a', 'b'}, TypeURL, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, rest, err := DecodeElement(tc.in)
			if tc.typ == -1 {
				tc.typ = TypeText
			}
			if err != nil || len(rest) != 0 {
				t.Fatalf("err=%v rest=%v", err, rest)
			}
			if e.Type != tc.typ || (tc.chk != nil && !tc.chk(e)) {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

func TestDecodeElementErrors(t *testing.T) {
	nested := []byte{0x08, 1}
	for i := 0; i < 12; i++ {
		nested = append([]byte{0x35, byte(len(nested))}, nested...)
	}
	tests := map[string][]byte{
		"empty":              {},
		"truncated value":    {0x0A, 1, 2},
		"truncated size":     {0x26, 0},
		"oversized length":   {0x27, 0xFF, 0xFF, 0xFF, 0xFF, 1},
		"seq length too big": {0x35, 9, 0x08},
		"child truncated":    {0x35, 2, 0x09, 1},
		"nil with size":      {0x01, 0},
		"bool size":          {0x29, 0, 0},
		"bool long":          {0x2D, 2, 0, 0},
		"uuid bad size":      {0x1B, 0, 0, 0, 0, 0, 0, 0, 0},
		"uuid 3 bytes":       {0x1D, 3, 1, 2, 3},
		"seq fixed size":     {0x31, 0, 0},
		"unknown type":       {0x4F, 0},
		"too deep":           nested,
		"int 0 bytes":        {0x0D, 0},
		"int 17 bytes":       append([]byte{0x0D, 17}, make([]byte, 17)...),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeElement(in); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := DecodeAll([]byte{0x08, 1, 0x08}); err == nil {
		t.Fatal("DecodeAll accepted trailing garbage")
	}
}

func TestDecodeNoPanic(t *testing.T) {
	for n := 0; n < 3000; n++ {
		b := make([]byte, 1+n%12)
		x := uint32(n)*2654435761 + 1
		for i := range b {
			x = x*1664525 + 1013904223
			b[i] = byte(x >> 24)
		}
		DecodeAll(b)
		ParseMASRecords(b)
		ParseSearchAttrResponse(b, 1, 1024)
	}
}

// --- record building helpers ---

func u8(v byte) []byte      { return []byte{0x08, v} }
func u16id(v uint16) []byte { return []byte{0x09, byte(v >> 8), byte(v)} }
func seq(parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	return append([]byte{0x35, byte(len(body))}, body...)
}

func record(name string, channel, inst, types byte, features []byte) []byte {
	var parts [][]byte
	if channel != 0 {
		parts = append(parts, u16id(0x0004), seq(seq([]byte{0x19, 0x01, 0x00}), seq([]byte{0x19, 0x00, 0x03}, u8(channel)), seq([]byte{0x19, 0x00, 0x08})))
	} else {
		parts = append(parts, u16id(0x0004), seq(seq([]byte{0x19, 0x01, 0x00}, []byte{0x09, 0, 0x1B})))
	}
	parts = append(parts, u16id(0x0100), append([]byte{0x25, byte(len(name))}, name...))
	parts = append(parts, u16id(0x0315), u8(inst), u16id(0x0316), u8(types))
	if features != nil {
		parts = append(parts, u16id(0x0317), append([]byte{0x0A}, features...))
	}
	return seq(parts...)
}

func TestRecordsAndChoice(t *testing.T) {
	email := record("Email", 5, 1, 0x01, nil)
	noChan := record("SMS-L2CAP", 0, 2, 0x02, nil)
	sms := record("SMS", 7, 3, 0x0A, []byte{0, 8, 0, 3})
	recs, err := ParseMASRecords(seq(email, noChan, sms))
	if err != nil || len(recs) != 3 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	if recs[0].Channel != 5 || recs[0].InstanceID != 1 || recs[0].Name != "Email" || recs[0].HasFeatures {
		t.Fatalf("email = %+v", recs[0])
	}
	if recs[1].Channel != 0 {
		t.Fatalf("no-channel record = %+v", recs[1])
	}
	want := MASRecord{Channel: 7, InstanceID: 3, MessageTypes: 0x0A, Features: 0x00080003, HasFeatures: true, Name: "SMS"}
	if recs[2] != want {
		t.Fatalf("sms = %+v want %+v", recs[2], want)
	}
	got, err := ChooseSMS(recs)
	if err != nil || got != want {
		t.Fatalf("choose = %+v, %v", got, err)
	}
	// CDMA also counts.
	if r, err := ChooseSMS([]MASRecord{{Channel: 2, MessageTypes: 0x04}}); err != nil || r.Channel != 2 {
		t.Fatalf("cdma: %+v %v", r, err)
	}
	if _, err := ChooseSMS(recs[:2]); !errors.Is(err, ErrNoSMS) {
		t.Fatalf("none: %v", err)
	}
	if _, err := ChooseSMS(nil); !errors.Is(err, ErrNoSMS) {
		t.Fatalf("empty: %v", err)
	}
	// SMS instance second in the list.
	r2, _ := ParseMASRecords(seq(email, sms))
	if c, _ := ChooseSMS(r2); c.InstanceID != 3 {
		t.Fatalf("second = %+v", c)
	}
	if r, err := ParseMASRecords(nil); err != nil || r != nil {
		t.Fatalf("nil input: %v %v", r, err)
	}
	if _, err := ParseMASRecords(u8(1)); err == nil {
		t.Fatal("non-sequence accepted")
	}
	if _, err := ParseMASRecords(seq(seq(u16id(0x0100)))); err == nil {
		t.Fatal("odd record accepted")
	}
	if _, err := ParseMASRecords(seq(seq(u8(1), u8(2)))); err == nil {
		t.Fatal("bad attribute id accepted")
	}
}

// --- Lookup ---

type fakeTransport struct {
	resps [][]byte // attribute fragments, one per round
	reqs  [][]byte
	n     int
	err   error
	loop  bool
}

func (f *fakeTransport) Exchange(ctx context.Context, req []byte) ([]byte, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	tid := uint16(req[1])<<8 | uint16(req[2])
	if f.loop {
		return resp(7, tid, attrParams([]byte{1}, []byte{0xAA})), nil
	}
	i := f.n
	f.n++
	var cont []byte
	if i < len(f.resps)-1 {
		cont = []byte{byte(i + 1), 0xEE}
	}
	return resp(7, tid, attrParams(f.resps[i], cont)), nil
}

func TestLookupContinuation(t *testing.T) {
	whole := seq(record("SMS", 7, 3, 0x02, nil))
	// Split inside the first data element header (after 1 byte) and in the middle.
	ft := &fakeTransport{resps: [][]byte{whole[:1], whole[1:10], whole[10:]}}
	got, err := Lookup(context.Background(), ft)
	if err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("got % x err %v", got, err)
	}
	if len(ft.reqs) != 3 {
		t.Fatalf("%d requests", len(ft.reqs))
	}
	// Continuation state is replayed opaquely.
	if tail := ft.reqs[1][len(ft.reqs[1])-3:]; !bytes.Equal(tail, []byte{2, 1, 0xEE}) {
		t.Fatalf("second request tail % x", tail)
	}
	recs, err := ParseMASRecords(got)
	if err != nil || len(recs) != 1 || recs[0].Channel != 7 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
}

func TestLookupLimits(t *testing.T) {
	ft := &fakeTransport{loop: true}
	if _, err := Lookup(context.Background(), ft); err == nil {
		t.Fatal("endless continuation accepted")
	}
	if len(ft.reqs) != 16 {
		t.Fatalf("%d rounds, want 16", len(ft.reqs))
	}
	// Byte limit: 1000-byte fragments, 66 of them would exceed 64 KiB, but the round limit is
	// 16; use maxTotal via a transport returning big fragments in a few rounds.
	big := make([]byte, 1024)
	frags := make([][]byte, 16)
	for i := range frags {
		frags[i] = big
	}
	ft = &fakeTransport{resps: frags}
	if got, err := Lookup(context.Background(), ft); err != nil || len(got) != 16*1024 {
		t.Fatalf("16 KiB: %d %v", len(got), err)
	}
}

func TestLookupByteLimit(t *testing.T) {
	frags := [][]byte{make([]byte, 600), make([]byte, 600), make([]byte, 600)}
	if _, err := lookup(context.Background(), &fakeTransport{resps: frags}, 16, 1500); err == nil {
		t.Fatal("byte limit not enforced")
	}
	if _, err := lookup(context.Background(), &fakeTransport{resps: frags}, 16, 1800); err != nil {
		t.Fatal(err)
	}
}

func TestLookupErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Lookup(context.Background(), &fakeTransport{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lookup(ctx, &fakeTransport{resps: [][]byte{nil}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	bad := &errTransport{}
	if _, err := Lookup(context.Background(), bad); err == nil {
		t.Fatal("wrong tid accepted")
	}
}

type errTransport struct{}

func (errTransport) Exchange(ctx context.Context, req []byte) ([]byte, error) {
	return resp(7, 999, attrParams(nil, nil)), nil
}
