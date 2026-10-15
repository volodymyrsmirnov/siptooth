package obex

import (
	"bytes"
	"fmt"
	"io"
	"time"
)

// PutHandler receives a completed Put (headers without Connection-ID, concatenated body)
// and returns the response code for the final packet.
type PutHandler func(hs Headers, body []byte) byte

// serverConnID is the Connection-ID siptooth allocates.
const serverConnID = 1

type putState struct {
	active   bool
	start    time.Time
	hs       Headers
	body     []byte
	hdrBytes int
	reject   byte
}

// Serve runs a Put-only OBEX server (the MNS, spec 17.5) on rwc and closes it on return.
// States: awaiting-connect then connected. onConnect (may be nil) is called once, after the
// Connect Success response is written. It returns nil after a Disconnect and otherwise the
// reason the connection ended (io.EOF for a peer close).
func Serve(rwc io.ReadWriteCloser, target []byte, h PutHandler, onConnect func()) error {
	c := NewConn(rwc, MaxPacket)
	defer c.Close()
	var (
		connected bool
		put       putState
	)
	reply := func(code byte, hs ...Header) error {
		b, err := Packet{Code: code, Headers: hs}.Marshal()
		if err != nil {
			return err
		}
		return c.Write(b)
	}
	for {
		wait := time.Duration(0) // idle: the phone keeps the connection open between messages
		if put.active {
			wait = packetTimeout
			if rem := time.Until(put.start.Add(operationTimeout)); rem <= 0 {
				return fmt.Errorf("%w: put deadline", ErrTimeout)
			} else if rem < wait {
				wait = rem
			}
		}
		b, err := c.Next(wait)
		if err != nil {
			return err
		}
		p, err := ParseRequest(b)
		if err != nil {
			return err
		}
		if !connected {
			if p.Code != OpConnect {
				if err := reply(RespBadRequest); err != nil {
					return err
				}
				continue
			}
			if t, ok := p.Headers.Get(HTarget); !ok || !bytes.Equal(t, target) {
				if err := reply(RespForbidden); err != nil {
					return err
				}
				continue
			}
			if p.Connect.MaxPacket < MinPeerPacket {
				if err := reply(RespBadRequest); err != nil {
					return err
				}
				continue
			}
			resp, err := Packet{Code: RespSuccess, Connect: &ConnectFields{Version: 0x10, MaxPacket: MaxPacket},
				Headers: Headers{U32(HConnectionID, serverConnID), Bytes(HWho, target)}}.Marshal()
			if err != nil {
				return err
			}
			if err := c.Write(resp); err != nil {
				return err
			}
			connected = true
			if onConnect != nil {
				onConnect()
			}
			continue
		}

		switch p.Code {
		case OpPut, OpPutFinal:
			if !put.active {
				put = putState{active: true, start: time.Now()}
			}
			if put.reject == 0 {
				for _, hd := range p.Headers {
					switch hd.ID {
					case HConnectionID:
						if len(hd.Value) != 4 || uint32(hd.Value[0])<<24|uint32(hd.Value[1])<<16|uint32(hd.Value[2])<<8|uint32(hd.Value[3]) != serverConnID {
							put.reject = RespForbidden
						}
					case HBody, HEndOfBody:
						put.body = append(put.body, hd.Value...)
					default:
						put.hs = append(put.hs, hd)
						put.hdrBytes += len(hd.Value) + 3
					}
				}
				if put.reject == 0 && (len(put.body) > MaxBody || put.hdrBytes > MaxHeaderBytes) {
					put.reject = RespEntityTooLarge
				}
				if put.reject != 0 {
					put.hs, put.body = nil, nil
				}
			}
			code := byte(RespContinue)
			if p.Code == OpPutFinal {
				switch {
				case put.reject != 0:
					code = put.reject
				case h == nil:
					code = RespNotImplemented
				default:
					code = h(put.hs, put.body)
				}
				put = putState{}
			} else if put.reject != 0 {
				code = put.reject
			}
			if err := reply(code); err != nil {
				return err
			}
		case OpAbort:
			put = putState{}
			if err := reply(RespSuccess); err != nil {
				return err
			}
		case OpDisconnect:
			if err := reply(RespSuccess); err != nil {
				return err
			}
			return nil
		case OpConnect:
			put = putState{}
			if err := reply(RespBadRequest); err != nil {
				return err
			}
		default:
			put = putState{}
			if err := reply(RespNotImplemented); err != nil {
				return err
			}
		}
	}
}
