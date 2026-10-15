package sms

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/btsdp"
	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

// MAP targets (spec 17.6): the OBEX Target UUIDs of the Message Access Server and the
// Message Notification Server.
var (
	MASTarget = []byte{0xBB, 0x58, 0x2B, 0x40, 0x42, 0x0C, 0x11, 0xDB, 0xB0, 0xDE, 0x08, 0x00, 0x20, 0x0C, 0x9A, 0x66}
	MNSTarget = []byte{0xBB, 0x58, 0x2B, 0x41, 0x42, 0x0C, 0x11, 0xDB, 0xB0, 0xDE, 0x08, 0x00, 0x20, 0x0C, 0x9A, 0x66}
)

// OwnFeatures is siptooth's MAP supported-features value: notification registration and
// notification (spec 17.6).
const OwnFeatures uint32 = 0x00000003

const (
	// featuresInConnect is bit 19 of a record's MapSupportedFeatures: "MapSupportedFeatures
	// in Connect Request" (MAP 1.4 6.4.1).
	featuresInConnect = 1 << 19

	tagMapSupportedFeatures = 0x29
	tagNotificationStatus   = 0x0E
	tagMASInstanceID        = 0x0F
	tagAttachment           = 0x0A
	tagCharset              = 0x14

	typeNotificationRegistration = "x-bt/MAP-NotificationRegistration"
	typeMessage                  = "x-bt/message"
)

// connectTimeout bounds the OBEX Connect to the MAS: Android holds the session until the
// owner answers its message-access prompt (about 25 s, spec 17.7).
const connectTimeout = 40 * time.Second

// MAS is an OBEX session to the phone's Message Access Server. RegisterNotifications and
// GetMessage are called only from the service's single MAS operation worker, never
// concurrently.
type MAS struct {
	c *obex.Client
}

// ConnectMAS runs the OBEX Connect (40 s) on rwc and closes rwc on failure. Cancelling ctx
// closes rwc at once. The MapSupportedFeatures parameter is sent only when the record
// advertises features with bit 19 set.
func ConnectMAS(ctx context.Context, rwc io.ReadWriteCloser, rec btsdp.MASRecord) (*MAS, error) {
	var ap []byte
	if rec.HasFeatures && rec.Features&featuresInConnect != 0 {
		ap = obex.EncodeAppParams(obex.AppParam{Tag: tagMapSupportedFeatures, Value: binary.BigEndian.AppendUint32(nil, OwnFeatures)})
	}
	c := obex.NewClient(rwc)
	if err := c.Connect(ctx, MASTarget, ap, connectTimeout); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sms: MAS connect: %w", err)
	}
	return &MAS{c: c}, nil
}

// RegisterNotifications turns event notifications on (SetNotificationRegistration).
func (m *MAS) RegisterNotifications() error {
	err := m.c.Put(obex.Headers{
		obex.ASCII(obex.HType, typeNotificationRegistration),
		obex.Bytes(obex.HAppParams, obex.EncodeAppParams(obex.AppParam{Tag: tagNotificationStatus, Value: []byte{1}})),
	}, []byte{0x30})
	if err != nil {
		return fmt.Errorf("sms: register notifications: %w", err)
	}
	return nil
}

// GetMessage fetches and parses the message with the given handle (UTF-8, no attachments).
// Classify the error with ClassifyGetError.
func (m *MAS) GetMessage(handle string) (BMessage, error) {
	body, err := m.c.Get(
		obex.Unicode(obex.HName, handle),
		obex.ASCII(obex.HType, typeMessage),
		obex.Bytes(obex.HAppParams, obex.EncodeAppParams(
			obex.AppParam{Tag: tagAttachment, Value: []byte{0}},
			obex.AppParam{Tag: tagCharset, Value: []byte{1}})),
	)
	if err != nil {
		return BMessage{}, fmt.Errorf("sms: get message: %w", err)
	}
	return ParseBMessage(body)
}

// Done is closed when the MAS connection is gone (an idle EOF included).
func (m *MAS) Done() <-chan struct{} { return m.c.Done() }

// Close closes the connection.
func (m *MAS) Close() error { return m.c.Close() }

// GetAction says what the service does with the result of a GetMessage (spec 17.7 step 5).
type GetAction int

const (
	// GetDeliver: the message was fetched and parsed.
	GetDeliver GetAction = iota
	// GetDrop: forget the handle, keep the session.
	GetDrop
	// GetRetry: try the handle again later (bounded by the service).
	GetRetry
	// GetEndSession: the MAS session is unusable and ends.
	GetEndSession
)

// ClassifyGetError maps a GetMessage error to its action. A nil error is GetDeliver; parse
// errors and negative responses drop the handle except Unauthorized (permission withdrawn:
// end the session) and Internal Error / Service Unavailable (retry); anything else, a
// connection-level failure, ends the session.
func ClassifyGetError(err error) GetAction {
	if err == nil {
		return GetDeliver
	}
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrMalformed) {
		return GetDrop
	}
	var re *obex.ResponseError
	if errors.As(err, &re) {
		switch re.Code {
		case obex.RespUnauthorized:
			return GetEndSession
		case obex.RespInternalError, obex.RespServiceUnavailable:
			return GetRetry
		default:
			return GetDrop
		}
	}
	return GetEndSession
}
