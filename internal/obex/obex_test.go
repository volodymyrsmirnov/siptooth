package obex

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func mustMarshal(t *testing.T, p Packet) []byte {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMarshalGolden(t *testing.T) {
	target := []byte{0xBB, 0x58, 0x2B, 0x40, 0x42, 0x0C, 0x11, 0xDB, 0xB0, 0xDE, 0x08, 0x00, 0x20, 0x0C, 0x9A, 0x66}
	tests := []struct {
		name string
		p    Packet
		want []byte
	}{
		{
			"connect request with target",
			Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: 8192}, Headers: Headers{Bytes(HTarget, target)}},
			append([]byte{0x80, 0x00, 0x1A, 0x10, 0x00, 0x20, 0x00, 0x46, 0x00, 0x13}, target...),
		},
		{
			"connect response with connection id and who",
			Packet{Code: RespSuccess, Connect: &ConnectFields{Version: 0x10, MaxPacket: 8192}, Headers: Headers{U32(HConnectionID, 1), Bytes(HWho, target)}},
			append([]byte{0xA0, 0x00, 0x1F, 0x10, 0x00, 0x20, 0x00, 0xCB, 0, 0, 0, 1, 0x4A, 0x00, 0x13}, target...),
		},
		{
			"final get",
			Packet{Code: OpGetFinal, Headers: Headers{
				U32(HConnectionID, 7), Unicode(HName, "ab"), ASCII(HType, "x/y"),
				Bytes(HAppParams, EncodeAppParams(AppParam{Tag: 1, Value: []byte{0x01}})),
			}},
			nil, // set below
		},
		{
			"final put with end of body",
			Packet{Code: OpPutFinal, Headers: Headers{Bytes(HEndOfBody, []byte("hi"))}},
			[]byte{0x82, 0x00, 0x08, 0x49, 0x00, 0x05, 'h', 'i'},
		},
	}
	// Final get: 3 + 5 (conn id) + 3+6 (name "ab\0") + 3+4 (type "x/y\0") + 3+3 (app params) = 30.
	tests[2].want = []byte{
		0x83, 0x00, 0x1E,
		0xCB, 0, 0, 0, 7,
		0x01, 0x00, 0x09, 0x00, 'a', 0x00, 'b', 0x00, 0x00,
		0x42, 0x00, 0x07, 'x', '/', 'y', 0x00,
		0x4C, 0x00, 0x06, 0x01, 0x01, 0x01,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustMarshal(t, tt.p)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("got % X\nwant % X", got, tt.want)
			}
		})
	}
}

func TestMarshalErrors(t *testing.T) {
	for name, h := range map[string]Header{
		"1-byte wrong length": {ID: 0x80, Value: []byte{1, 2}},
		"4-byte wrong length": {ID: HConnectionID, Value: []byte{1}},
		"odd unicode":         {ID: HName, Value: []byte{1}},
	} {
		if _, err := (Packet{Code: OpPutFinal, Headers: Headers{h}}).Marshal(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	big := Bytes(HBody, make([]byte, 40000))
	if _, err := (Packet{Code: OpPut, Headers: Headers{big, big}}).Marshal(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyNameGolden(t *testing.T) {
	b := mustMarshal(t, Packet{Code: OpGetFinal, Headers: Headers{Unicode(HName, "")}})
	if want := []byte{0x83, 0x00, 0x06, 0x01, 0x00, 0x03}; !bytes.Equal(b, want) {
		t.Fatalf("got % X", b)
	}
	p, err := ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := p.Headers.Get(HName)
	if !ok {
		t.Fatal("no name")
	}
	if s, err := DecodeUnicode(v); err != nil || s != "" {
		t.Fatalf("got %q, %v", s, err)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	hs := Headers{
		Unicode(HName, "Привіт, світе"),
		Unicode(HName, "tel"),
		ASCII(HType, "x-bt/MAP-event-report"),
		Bytes(HBody, []byte{0, 1, 2}),
		Header{ID: 0x93, Value: []byte{9}}, // unknown 1-byte
		Header{ID: 0xC5, Value: []byte{0, 0, 1, 0}},
		Header{ID: 0x77, Value: []byte("unknown")},
		U32(HConnectionID, 0xDEADBEEF),
	}
	b := mustMarshal(t, Packet{Code: OpPutFinal, Headers: hs})
	p, err := ParseRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Headers, hs) {
		t.Fatalf("got %v", p.Headers)
	}
	v, _ := p.Headers.Get(HName)
	if s, _ := DecodeUnicode(v); s != "Привіт, світе" {
		t.Fatalf("name %q", s)
	}
	v, _ = p.Headers.Get(HType)
	if DecodeASCII(v) != "x-bt/MAP-event-report" {
		t.Fatal("type")
	}
	if _, ok := p.Headers.Get(0x55); ok {
		t.Fatal("unexpected header")
	}
	if DecodeASCII([]byte("a")) != "a" || DecodeASCII(nil) != "" {
		t.Fatal("DecodeASCII without NUL")
	}
}

func TestDecodeUnicodeOdd(t *testing.T) {
	if _, err := DecodeUnicode([]byte{0, 'a', 0}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}

func TestConnectParse(t *testing.T) {
	b := []byte{0x80, 0x00, 0x07, 0x10, 0x00, 0x20, 0x00}
	p, err := ParseRequest(b)
	if err != nil || p.Connect == nil || *p.Connect != (ConnectFields{0x10, 0, 8192}) {
		t.Fatalf("got %+v, %v", p, err)
	}
	r, err := ParseResponse([]byte{0xA0, 0x00, 0x07, 0x10, 0x00, 0x01, 0x00}, true)
	if err != nil || r.Connect.MaxPacket != 256 {
		t.Fatalf("got %+v, %v", r, err)
	}
	r, err = ParseResponse([]byte{0xA0, 0x00, 0x03}, false)
	if err != nil || r.Connect != nil || r.Code != RespSuccess {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string][]byte{
		"empty":                  nil,
		"two bytes":              {0x82, 0x00},
		"length too long":        {0x82, 0x00, 0x09, 0x49, 0x00, 0x03},
		"length too short":       {0x82, 0x00, 0x03, 0x49, 0x00, 0x03},
		"header length below 3":  {0x82, 0x00, 0x06, 0x49, 0x00, 0x02},
		"header past end":        {0x82, 0x00, 0x08, 0x49, 0x00, 0x09, 'h', 'i'},
		"header id only":         {0x82, 0x00, 0x04, 0x49},
		"1-byte header past end": {0x82, 0x00, 0x04, 0x93},
		"4-byte header short":    {0x82, 0x00, 0x07, 0xCB, 0, 0, 1},
		"odd unicode":            {0x82, 0x00, 0x07, 0x01, 0x00, 0x04, 0x00},
		"short connect fields":   {0x80, 0x00, 0x06, 0x10, 0x00, 0x20},
	}
	for name, b := range tests {
		if _, err := ParseRequest(b); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := ParseResponse([]byte{0xA0, 0x00, 0x05, 0x10, 0x00}, true); !errors.Is(err, ErrProtocol) {
		t.Errorf("response connect fields: got %v", err)
	}
}

func TestAppParams(t *testing.T) {
	in := []AppParam{{0x01, []byte{0xFF}}, {0x04, []byte{0, 0, 0, 3}}, {0x09, nil}}
	b := EncodeAppParams(in...)
	if want := []byte{1, 1, 0xFF, 4, 4, 0, 0, 0, 3, 9, 0}; !bytes.Equal(b, want) {
		t.Fatalf("got % X", b)
	}
	out, err := DecodeAppParams(b)
	if err != nil || len(out) != 3 || out[1].Tag != 4 || !bytes.Equal(out[1].Value, []byte{0, 0, 0, 3}) || len(out[2].Value) != 0 {
		t.Fatalf("got %+v, %v", out, err)
	}
	if out, err := DecodeAppParams(nil); err != nil || len(out) != 0 {
		t.Fatalf("empty: %v %v", out, err)
	}
	for _, bad := range [][]byte{{1}, {1, 2, 3}} {
		if _, err := DecodeAppParams(bad); !errors.Is(err, ErrProtocol) {
			t.Errorf("% X: got %v", bad, err)
		}
	}
}

func TestParseResponseBareConnectRefusal(t *testing.T) {
	p, err := ParseResponse([]byte{0xC3, 0x00, 0x03}, true)
	if err != nil || p.Code != RespForbidden || p.Connect != nil {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestParseResponseRefusalShapes(t *testing.T) {
	// With Connect fields and a header.
	p, err := ParseResponse([]byte{0xC3, 0x00, 0x0C, 0x10, 0x00, 0x01, 0x00, 0xCB, 0, 0, 0, 1}, true)
	if err != nil || p.Code != RespForbidden || p.Connect == nil || len(p.Headers) != 1 {
		t.Fatalf("got %+v", p)
	}
	// Without Connect fields but with a header, 8 bytes: the Connect parse fails, the plain one works.
	p, err = ParseResponse([]byte{0xC3, 0x00, 0x08, 0xCB, 0x00, 0x00, 0x00, 0x01}, true)
	if err != nil || p.Code != RespForbidden || p.Connect != nil || len(p.Headers) != 1 {
		t.Fatalf("got %+v, %v", p, err)
	}
	// Proper Connect fields on a refusal are kept.
	p, err = ParseResponse([]byte{0xC3, 0x00, 0x07, 0x10, 0x00, 0x01, 0x00}, true)
	if err != nil || p.Code != RespForbidden || p.Connect == nil {
		t.Fatalf("got %+v, %v", p, err)
	}
}
