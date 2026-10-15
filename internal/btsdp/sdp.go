package btsdp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	pduErrorResponse      = 0x01
	pduSearchAttrRequest  = 0x06
	pduSearchAttrResponse = 0x07

	// Lookup limits (spec 17.4).
	maxCount     = 1024
	maxRounds    = 16
	maxTotal     = 64 << 10
	maxContState = 16

	uuidMAS = 0x1132
)

// ErrorResponse is an SDP_ErrorResponse PDU from the phone.
type ErrorResponse struct{ Code uint16 }

func (e *ErrorResponse) Error() string {
	return fmt.Sprintf("btsdp: SDP error response 0x%04x", e.Code)
}

// Transport carries one SDP request/response exchange (an L2CAP socket on Linux, a fake in tests).
type Transport interface {
	Exchange(ctx context.Context, req []byte) ([]byte, error)
}

// EncodeSearchAttrRequest builds an SDP_ServiceSearchAttributeRequest for one 16-bit UUID, all
// attributes (0x0000-0xFFFF), a 1024 byte attribute budget and the given continuation state.
func EncodeSearchAttrRequest(tid uint16, uuid16 uint16, cont []byte) []byte {
	p := []byte{
		0x35, 0x03, 0x19, byte(uuid16 >> 8), byte(uuid16), // pattern: DES{UUID16}
		byte(maxCount >> 8), byte(maxCount & 0xff), // maximum attribute byte count
		0x35, 0x05, 0x0A, 0x00, 0x00, 0xFF, 0xFF, // attribute ID list: DES{uint32 range}
		byte(len(cont)),
	}
	p = append(p, cont...)
	out := []byte{pduSearchAttrRequest, byte(tid >> 8), byte(tid), byte(len(p) >> 8), byte(len(p))}
	return append(out, p...)
}

// ParseSearchAttrResponse validates one response PDU and returns its attribute bytes and the
// continuation state (empty when this is the last fragment). maxCount is the byte budget that was
// requested. An SDP error PDU becomes *ErrorResponse.
func ParseSearchAttrResponse(b []byte, wantTID uint16, maxCount uint16) (attrBytes, cont []byte, err error) {
	if len(b) < 5 {
		return nil, nil, errors.New("btsdp: short response")
	}
	pdu := b[0]
	tid := binary.BigEndian.Uint16(b[1:3])
	plen := int(binary.BigEndian.Uint16(b[3:5]))
	params := b[5:]
	if pdu != pduSearchAttrResponse && pdu != pduErrorResponse {
		return nil, nil, fmt.Errorf("btsdp: unexpected PDU 0x%02x", pdu)
	}
	if tid != wantTID {
		return nil, nil, fmt.Errorf("btsdp: transaction id %d, want %d", tid, wantTID)
	}
	if plen != len(params) {
		return nil, nil, fmt.Errorf("btsdp: parameter length %d, %d bytes follow", plen, len(params))
	}
	if pdu == pduErrorResponse {
		if len(params) < 2 {
			return nil, nil, errors.New("btsdp: short error response")
		}
		return nil, nil, &ErrorResponse{Code: binary.BigEndian.Uint16(params)}
	}
	if len(params) < 3 {
		return nil, nil, errors.New("btsdp: short response parameters")
	}
	count := int(binary.BigEndian.Uint16(params))
	if count > int(maxCount) {
		return nil, nil, fmt.Errorf("btsdp: attribute byte count %d over the requested %d", count, maxCount)
	}
	if 2+count+1 > len(params) {
		return nil, nil, fmt.Errorf("btsdp: attribute byte count %d does not fit the parameters", count)
	}
	attrBytes = params[2 : 2+count]
	rest := params[2+count:]
	cl := int(rest[0])
	if cl > maxContState {
		return nil, nil, fmt.Errorf("btsdp: continuation state of %d bytes", cl)
	}
	if len(rest) != 1+cl {
		return nil, nil, errors.New("btsdp: continuation state does not match the parameters")
	}
	return attrBytes, rest[1:], nil
}

// Lookup runs the MAS service search with the continuation loop and returns the concatenated raw
// attribute bytes. It decodes nothing: a fragment may split a data element anywhere.
func Lookup(ctx context.Context, t Transport) ([]byte, error) {
	return lookup(ctx, t, maxRounds, maxTotal)
}

func lookup(ctx context.Context, t Transport, rounds, total int) ([]byte, error) {
	var all, cont []byte
	for round := 0; round < rounds; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tid := uint16(round + 1)
		resp, err := t.Exchange(ctx, EncodeSearchAttrRequest(tid, uuidMAS, cont))
		if err != nil {
			return nil, err
		}
		attrs, next, err := ParseSearchAttrResponse(resp, tid, maxCount)
		if err != nil {
			return nil, err
		}
		if len(all)+len(attrs) > total {
			return nil, fmt.Errorf("btsdp: response over %d bytes", total)
		}
		all = append(all, attrs...)
		if len(next) == 0 {
			return all, nil
		}
		cont = append([]byte(nil), next...)
	}
	return nil, fmt.Errorf("btsdp: no end of response after %d rounds", rounds)
}
