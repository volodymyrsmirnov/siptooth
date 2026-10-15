// Package obex implements the small OBEX subset siptooth needs for Bluetooth MAP (spec 17.5):
// the packet and header codec, stream framing over RFCOMM, a client (Connect, Get, Put,
// Disconnect) and a Put-only server for the MNS notification channel.
//
// Errors never carry payload bytes: message text and event reports stay out of logs.
package obex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

// Opcodes (request) and response codes.
const (
	OpConnect    = 0x80
	OpDisconnect = 0x81
	OpPut        = 0x02
	OpPutFinal   = 0x82
	OpGetFinal   = 0x83
	OpAbort      = 0xFF

	RespContinue           = 0x90
	RespSuccess            = 0xA0
	RespBadRequest         = 0xC0
	RespUnauthorized       = 0xC1
	RespForbidden          = 0xC3
	RespNotFound           = 0xC4
	RespNotAcceptable      = 0xC6
	RespEntityTooLarge     = 0xCD
	RespInternalError      = 0xD0
	RespNotImplemented     = 0xD1
	RespServiceUnavailable = 0xD3
)

// Header IDs. The two high bits select the value encoding.
const (
	HName         = 0x01
	HType         = 0x42
	HTarget       = 0x46
	HWho          = 0x4A
	HBody         = 0x48
	HEndOfBody    = 0x49
	HAppParams    = 0x4C
	HConnectionID = 0xCB
)

// Limits and deadlines.
const (
	// MaxPacket is the maximum packet length siptooth advertises.
	MaxPacket = 8192
	// MinPeerPacket is the smallest maximum packet length a peer may advertise.
	MinPeerPacket = 255
	// MaxBody is the cap on an accumulated Body; MaxHeaderBytes the cap on other headers.
	MaxBody        = 1 << 20
	MaxHeaderBytes = 64 << 10

	PacketTimeout    = 10 * time.Second
	OperationTimeout = 30 * time.Second
	AbortWait        = 2 * time.Second
)

// Tests shorten these; production code never changes them.
var (
	packetTimeout    = PacketTimeout
	operationTimeout = OperationTimeout
	abortWait        = AbortWait
)

// ErrProtocol is wrapped (with a detail, never payload bytes) by every protocol violation.
var ErrProtocol = errors.New("obex: protocol error")

// ErrTimeout is wrapped by every deadline expiry.
var ErrTimeout = errors.New("obex: timeout")

// ErrTooLarge is returned when an answer exceeds the body/header caps or a request does not
// fit the peer's maximum packet length.
var ErrTooLarge = errors.New("obex: too large")

// ResponseError reports a response code other than Success (or Continue where allowed).
type ResponseError struct{ Code byte }

func (e *ResponseError) Error() string { return fmt.Sprintf("obex: response 0x%02X", e.Code) }

func protocolf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrProtocol}, args...)...)
}

// Header is one OBEX header. Value is the raw value: byte-seq headers as is, unicode
// headers as UTF-16BE including the trailing 0x0000 (empty for an empty string), 1-byte and
// 4-byte headers as the big-endian value bytes.
type Header struct {
	ID    byte
	Value []byte
}

// Headers is an ordered header list.
type Headers []Header

// Get returns the value of the first header with the given ID.
func (hs Headers) Get(id byte) ([]byte, bool) {
	for _, h := range hs {
		if h.ID == id {
			return h.Value, true
		}
	}
	return nil, false
}

// Unicode builds a unicode header; the empty string encodes as a zero-length value.
func Unicode(id byte, s string) Header {
	if s == "" {
		return Header{ID: id}
	}
	u := utf16.Encode([]rune(s))
	v := make([]byte, 0, 2*len(u)+2)
	for _, c := range u {
		v = binary.BigEndian.AppendUint16(v, c)
	}
	return Header{ID: id, Value: append(v, 0, 0)}
}

// ASCII builds a byte-seq header holding s and a trailing NUL (Type).
func ASCII(id byte, s string) Header {
	return Header{ID: id, Value: append([]byte(s), 0)}
}

// Bytes builds a byte-seq header.
func Bytes(id byte, b []byte) Header { return Header{ID: id, Value: b} }

// U32 builds a 4-byte header.
func U32(id byte, v uint32) Header {
	return Header{ID: id, Value: binary.BigEndian.AppendUint32(nil, v)}
}

// DecodeUnicode decodes a unicode header value, dropping the terminator.
func DecodeUnicode(v []byte) (string, error) {
	if len(v)%2 != 0 {
		return "", protocolf("odd unicode length")
	}
	u := make([]uint16, 0, len(v)/2)
	for i := 0; i < len(v); i += 2 {
		u = append(u, binary.BigEndian.Uint16(v[i:]))
	}
	if n := len(u); n > 0 && u[n-1] == 0 {
		u = u[:n-1]
	}
	return string(utf16.Decode(u)), nil
}

// DecodeASCII strips one trailing NUL.
func DecodeASCII(v []byte) string {
	if n := len(v); n > 0 && v[n-1] == 0 {
		v = v[:n-1]
	}
	return string(v)
}

// AppParam is one Application-Parameters TLV.
type AppParam struct {
	Tag   byte
	Value []byte
}

// EncodeAppParams encodes TLVs (tag, 1-byte length, value). A value longer than 255 bytes
// is truncated; no siptooth parameter comes close.
func EncodeAppParams(ps ...AppParam) []byte {
	var b []byte
	for _, p := range ps {
		v := p.Value
		if len(v) > 255 {
			v = v[:255]
		}
		b = append(b, p.Tag, byte(len(v)))
		b = append(b, v...)
	}
	return b
}

// DecodeAppParams parses Application-Parameters TLVs.
func DecodeAppParams(b []byte) ([]AppParam, error) {
	var ps []AppParam
	for len(b) > 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return nil, protocolf("truncated app parameter")
		}
		n := int(b[1])
		ps = append(ps, AppParam{Tag: b[0], Value: append([]byte(nil), b[2:2+n]...)})
		b = b[2+n:]
	}
	return ps, nil
}

// ConnectFields are the extra fields of a Connect request and its response.
type ConnectFields struct {
	Version, Flags byte
	MaxPacket      uint16
}

// Packet is one OBEX packet. Connect is set for Connect requests and their responses.
type Packet struct {
	Code    byte
	Connect *ConnectFields
	Headers Headers
}

// Marshal encodes the packet.
func (p Packet) Marshal() ([]byte, error) {
	b := []byte{p.Code, 0, 0}
	if p.Connect != nil {
		b = append(b, p.Connect.Version, p.Connect.Flags, byte(p.Connect.MaxPacket>>8), byte(p.Connect.MaxPacket))
	}
	for _, h := range p.Headers {
		switch h.ID >> 6 {
		case 0, 1:
			if h.ID>>6 == 0 && len(h.Value)%2 != 0 {
				return nil, fmt.Errorf("obex: header 0x%02X: odd unicode length", h.ID)
			}
			if len(h.Value)+3 > 0xFFFF {
				return nil, fmt.Errorf("obex: header 0x%02X: %w", h.ID, ErrTooLarge)
			}
			b = append(b, h.ID, byte((len(h.Value)+3)>>8), byte(len(h.Value)+3))
		case 2:
			if len(h.Value) != 1 {
				return nil, fmt.Errorf("obex: header 0x%02X: want 1 byte", h.ID)
			}
			b = append(b, h.ID)
		case 3:
			if len(h.Value) != 4 {
				return nil, fmt.Errorf("obex: header 0x%02X: want 4 bytes", h.ID)
			}
			b = append(b, h.ID)
		}
		b = append(b, h.Value...)
	}
	if len(b) > 0xFFFF {
		return nil, fmt.Errorf("obex: packet: %w", ErrTooLarge)
	}
	binary.BigEndian.PutUint16(b[1:], uint16(len(b)))
	return b, nil
}

// ParseRequest parses a request packet (Connect fields when Code == OpConnect).
func ParseRequest(b []byte) (Packet, error) { return parse(b, len(b) > 0 && b[0] == OpConnect) }

// ParseResponse parses a response; toConnect says it answers a Connect.
// A refusal to a Connect may lack the Connect fields (a bare 3-byte packet or only headers).
func ParseResponse(b []byte, toConnect bool) (Packet, error) {
	if toConnect && len(b) > 0 && b[0] != RespSuccess {
		// A refusal may carry the Connect fields or not; try with them first.
		if p, err := parse(b, true); err == nil {
			return p, nil
		}
		return parse(b, false)
	}
	return parse(b, toConnect)
}

func parse(b []byte, connect bool) (Packet, error) {
	if len(b) < 3 {
		return Packet{}, protocolf("short packet")
	}
	if int(binary.BigEndian.Uint16(b[1:])) != len(b) {
		return Packet{}, protocolf("length field mismatch")
	}
	p := Packet{Code: b[0]}
	rest := b[3:]
	if connect {
		if len(rest) < 4 {
			return Packet{}, protocolf("short connect fields")
		}
		p.Connect = &ConnectFields{Version: rest[0], Flags: rest[1], MaxPacket: binary.BigEndian.Uint16(rest[2:])}
		rest = rest[4:]
	}
	for len(rest) > 0 {
		id := rest[0]
		var n, hdr int
		switch id >> 6 {
		case 0, 1:
			if len(rest) < 3 {
				return Packet{}, protocolf("header past end")
			}
			l := int(binary.BigEndian.Uint16(rest[1:]))
			if l < 3 {
				return Packet{}, protocolf("header length below 3")
			}
			hdr, n = 3, l-3
		case 2:
			hdr, n = 1, 1
		case 3:
			hdr, n = 1, 4
		}
		if len(rest) < hdr+n {
			return Packet{}, protocolf("header past end")
		}
		v := append([]byte(nil), rest[hdr:hdr+n]...)
		if id>>6 == 0 && n%2 != 0 {
			return Packet{}, protocolf("odd unicode length")
		}
		p.Headers = append(p.Headers, Header{ID: id, Value: v})
		rest = rest[hdr+n:]
	}
	return p, nil
}
