// Package btsdp is siptooth's own minimal Bluetooth SDP client (spec 17.4): it asks the phone for
// its Message Access Server (MAS) records over L2CAP PSM 1 and picks the SMS instance, and it dials
// the chosen RFCOMM channel. The codec is platform independent; the sockets are Linux only.
package btsdp

import (
	"errors"
	"fmt"
)

// Data element types (the high five bits of the header byte).
const (
	TypeNil  = 0
	TypeUint = 1
	TypeSint = 2
	TypeUUID = 3
	TypeText = 4
	TypeBool = 5
	TypeSeq  = 6
	TypeAlt  = 7
	TypeURL  = 8
)

const maxDepth = 8

// Element is one decoded SDP data element.
type Element struct {
	Type     int
	Raw      []byte // the value bytes (integers and UUIDs big endian, text/URL as sent)
	Uint     uint64 // unsigned value (Type uint, up to 8 bytes) or UUID value (16 and 32 bit forms)
	Bool     bool
	Children []Element // sequence and alternative members
}

// ErrMalformed is wrapped by every data-element decode failure.
var ErrMalformed = errors.New("btsdp: malformed data element")

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, a...))
}

// DecodeElement decodes one data element from the start of b and returns the bytes after it.
func DecodeElement(b []byte) (Element, []byte, error) {
	return decodeElement(b, 0)
}

// DecodeAll decodes b as a series of data elements that must fill it exactly.
func DecodeAll(b []byte) ([]Element, error) {
	var out []Element
	for len(b) > 0 {
		e, rest, err := decodeElement(b, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		b = rest
	}
	return out, nil
}

func decodeElement(b []byte, depth int) (Element, []byte, error) {
	if depth > maxDepth {
		return Element{}, nil, malformed("nesting deeper than %d", maxDepth)
	}
	if len(b) < 1 {
		return Element{}, nil, malformed("empty element")
	}
	typ := int(b[0] >> 3)
	sd := int(b[0] & 7)
	hdr := 1
	var n uint64
	switch sd {
	case 0:
		if typ == TypeNil {
			n = 0
		} else {
			n = 1
		}
	case 1:
		n = 2
	case 2:
		n = 4
	case 3:
		n = 8
	case 4:
		n = 16
	case 5, 6, 7:
		w := 1 << (sd - 5)
		if len(b) < 1+w {
			return Element{}, nil, malformed("truncated size field")
		}
		for i := 0; i < w; i++ {
			n = n<<8 | uint64(b[1+i])
		}
		hdr += w
	}
	if typ > TypeURL {
		return Element{}, nil, malformed("unknown type %d", typ)
	}
	if sd < 5 {
		// Fixed-size descriptors: nil takes size index 0 only; sequences, alternatives, text and
		// URLs always carry an explicit length.
		switch typ {
		case TypeNil:
			if sd != 0 {
				return Element{}, nil, malformed("nil with size index %d", sd)
			}
		case TypeSeq, TypeAlt, TypeText, TypeURL:
			return Element{}, nil, malformed("type %d needs a length field", typ)
		case TypeBool:
			if sd != 0 {
				return Element{}, nil, malformed("bool with size index %d", sd)
			}
		case TypeUUID:
			if sd != 1 && sd != 2 && sd != 4 {
				return Element{}, nil, malformed("uuid with size index %d", sd)
			}
		}
	}
	if typ == TypeNil && sd != 0 {
		return Element{}, nil, malformed("nil with a length field")
	}
	if n > uint64(len(b)-hdr) {
		return Element{}, nil, malformed("length %d exceeds the %d bytes left", n, len(b)-hdr)
	}
	body := b[hdr : hdr+int(n)]
	rest := b[hdr+int(n):]
	e := Element{Type: typ, Raw: body}
	switch typ {
	case TypeUint, TypeSint:
		if sd >= 5 && (n < 1 || n > 16) {
			return Element{}, nil, malformed("integer of %d bytes", n)
		}
		if typ == TypeUint && n <= 8 {
			for _, c := range body {
				e.Uint = e.Uint<<8 | uint64(c)
			}
		}
	case TypeUUID:
		if n != 2 && n != 4 && n != 16 {
			return Element{}, nil, malformed("uuid of %d bytes", n)
		}
		if n <= 4 {
			for _, c := range body {
				e.Uint = e.Uint<<8 | uint64(c)
			}
		}
	case TypeBool:
		if n != 1 {
			return Element{}, nil, malformed("bool of %d bytes", n)
		}
		e.Bool = body[0] != 0
	case TypeSeq, TypeAlt:
		for len(body) > 0 {
			c, r, err := decodeElement(body, depth+1)
			if err != nil {
				return Element{}, nil, err
			}
			e.Children = append(e.Children, c)
			body = r
		}
	}
	return e, rest, nil
}

// Text returns the element's text, for text and URL elements only.
func (e Element) Text() (string, bool) {
	if e.Type != TypeText && e.Type != TypeURL {
		return "", false
	}
	s := e.Raw
	for len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return string(s), true
}
