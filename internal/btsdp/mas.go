package btsdp

import (
	"errors"
	"fmt"
)

// Attribute IDs and values of a MAS record.
const (
	attrProtocolDescList = 0x0004
	attrServiceName      = 0x0100
	attrMASInstanceID    = 0x0315
	attrSupportedMsgType = 0x0316
	attrMapFeatures      = 0x0317

	uuidL2CAP  = 0x0100
	uuidRFCOMM = 0x0003

	// SupportedMessageTypes bits.
	MsgTypeSMSGSM  = 0x02
	MsgTypeSMSCDMA = 0x04
)

// ErrNoSMS means the phone offers no SMS message access over RFCOMM.
var ErrNoSMS = errors.New("phone offers no SMS message access over RFCOMM")

// MASRecord is the part of an SDP MAS record siptooth uses.
type MASRecord struct {
	Channel      uint8 // RFCOMM channel, 0 when the record has none
	InstanceID   uint8
	MessageTypes uint8
	Features     uint32
	HasFeatures  bool
	Name         string
}

// ParseMASRecords decodes the concatenated attribute bytes of a search response (a sequence of
// per-record attribute sequences) into records, in response order.
func ParseMASRecords(attrLists []byte) ([]MASRecord, error) {
	top, err := DecodeAll(attrLists)
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, nil
	}
	if len(top) != 1 || top[0].Type != TypeSeq {
		return nil, malformed("response is not one sequence of records")
	}
	var recs []MASRecord
	for _, rec := range top[0].Children {
		if rec.Type != TypeSeq {
			return nil, malformed("record is not a sequence")
		}
		if len(rec.Children)%2 != 0 {
			return nil, malformed("record has an odd number of elements")
		}
		var r MASRecord
		for i := 0; i < len(rec.Children); i += 2 {
			id, val := rec.Children[i], rec.Children[i+1]
			if id.Type != TypeUint || len(id.Raw) != 2 {
				return nil, malformed("attribute id is not a uint16")
			}
			switch id.Uint {
			case attrProtocolDescList:
				r.Channel = rfcommChannel(val)
			case attrServiceName:
				if s, ok := val.Text(); ok {
					r.Name = s
				}
			case attrMASInstanceID:
				if val.Type == TypeUint && len(val.Raw) == 1 {
					r.InstanceID = uint8(val.Uint)
				}
			case attrSupportedMsgType:
				if val.Type == TypeUint && len(val.Raw) == 1 {
					r.MessageTypes = uint8(val.Uint)
				}
			case attrMapFeatures:
				if val.Type == TypeUint && len(val.Raw) == 4 {
					r.Features = uint32(val.Uint)
					r.HasFeatures = true
				}
			}
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// rfcommChannel walks a ProtocolDescriptorList (L2CAP, then RFCOMM with a uint8 channel).
func rfcommChannel(pdl Element) uint8 {
	if pdl.Type != TypeSeq {
		return 0
	}
	sawL2CAP := false
	for _, proto := range pdl.Children {
		if proto.Type != TypeSeq || len(proto.Children) == 0 || proto.Children[0].Type != TypeUUID {
			continue
		}
		switch proto.Children[0].Uint {
		case uuidL2CAP:
			if len(proto.Children[0].Raw) == 2 {
				sawL2CAP = true
			}
		case uuidRFCOMM:
			if sawL2CAP && len(proto.Children) > 1 {
				p := proto.Children[1]
				if p.Type == TypeUint && len(p.Raw) == 1 {
					return uint8(p.Uint)
				}
			}
		}
	}
	return 0
}

// ChooseSMS returns the first record with an RFCOMM channel that supports SMS_GSM or SMS_CDMA.
func ChooseSMS(recs []MASRecord) (MASRecord, error) {
	for _, r := range recs {
		if r.Channel != 0 && r.MessageTypes&(MsgTypeSMSGSM|MsgTypeSMSCDMA) != 0 {
			return r, nil
		}
	}
	return MASRecord{}, ErrNoSMS
}

func (r MASRecord) String() string {
	return fmt.Sprintf("instance %d channel %d", r.InstanceID, r.Channel)
}
