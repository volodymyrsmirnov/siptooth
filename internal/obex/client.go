package obex

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Client is an OBEX client over a byte stream (spec 17.5). It is not safe for overlapping
// operations: its owner runs one at a time. Any deadline or protocol error closes the
// connection (OBEX has no resynchronisation).
type Client struct {
	c       *Conn
	connID  uint32
	haveID  bool
	peerMax int
}

// NewClient wraps the transport and starts its reader.
func NewClient(rwc io.ReadWriteCloser) *Client {
	return &Client{c: NewConn(rwc, MaxPacket), peerMax: MinPeerPacket}
}

// fail closes the connection and returns err.
func (c *Client) fail(err error) error {
	_ = c.c.Close()
	return err
}

// Connect performs the OBEX Connect with the given Target and optional Application-Parameters
// (nil: no header). timeout (<= 0: none) bounds the wait for the response. Cancelling ctx
// closes the transport at once and returns ctx.Err(). A response other than Success is a
// *ResponseError; the connection is then left to the caller to Close.
func (c *Client) Connect(ctx context.Context, target []byte, appParams []byte, timeout time.Duration) error {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = c.c.Close()
		case <-stop:
		}
	}()
	err := c.connect(target, appParams, timeout)
	close(stop)
	<-finished
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			_ = c.c.Close()
			return cerr
		}
	}
	return err
}

func (c *Client) connect(target, appParams []byte, timeout time.Duration) error {
	hs := Headers{Bytes(HTarget, target)}
	if appParams != nil {
		hs = append(hs, Bytes(HAppParams, appParams))
	}
	req, err := Packet{Code: OpConnect, Connect: &ConnectFields{Version: 0x10, MaxPacket: MaxPacket}, Headers: hs}.Marshal()
	if err != nil {
		return err
	}
	if err := c.c.Write(req); err != nil {
		return c.fail(err)
	}
	b, err := c.c.Next(timeout)
	if err != nil {
		return c.fail(err)
	}
	p, err := ParseResponse(b, true)
	if err != nil {
		return c.fail(err)
	}
	if p.Code&0x80 == 0 {
		return c.fail(protocolf("invalid response code"))
	}
	if p.Code != RespSuccess {
		return &ResponseError{Code: p.Code}
	}
	if p.Connect == nil {
		return c.fail(protocolf("missing connect fields"))
	}
	if int(p.Connect.MaxPacket) < MinPeerPacket {
		return c.fail(protocolf("peer packet size below %d", MinPeerPacket))
	}
	c.peerMax = int(p.Connect.MaxPacket)
	if v, ok := p.Headers.Get(HConnectionID); ok && len(v) == 4 {
		c.connID, c.haveID = binary.BigEndian.Uint32(v), true
	}
	return nil
}

// request marshals a request with the Connection-ID first.
func (c *Client) request(code byte, hs Headers) ([]byte, error) {
	var all Headers
	if c.haveID {
		all = append(all, U32(HConnectionID, c.connID))
	}
	all = append(all, hs...)
	b, err := Packet{Code: code, Headers: all}.Marshal()
	if err != nil {
		return nil, err
	}
	if len(b) > c.peerMax {
		return nil, fmt.Errorf("request does not fit the peer packet size: %w", ErrTooLarge)
	}
	return b, nil
}

// final waits for the single final response of Put and Disconnect.
func (c *Client) final(req []byte) (Packet, error) {
	if err := c.c.Write(req); err != nil {
		return Packet{}, c.fail(err)
	}
	b, err := c.c.Next(packetTimeout)
	if err != nil {
		return Packet{}, c.fail(err)
	}
	p, err := ParseResponse(b, false)
	if err != nil {
		return Packet{}, c.fail(err)
	}
	if p.Code&0x80 == 0 || p.Code == RespContinue {
		return Packet{}, c.fail(protocolf("non-final response code"))
	}
	if p.Code != RespSuccess {
		return Packet{}, &ResponseError{Code: p.Code}
	}
	return p, nil
}

// Put sends hs plus End-of-Body(body) in one final Put. A request larger than the peer's
// maximum packet is ErrTooLarge (nothing sent, the connection stays usable).
func (c *Client) Put(hs Headers, body []byte) error {
	all := append(append(Headers(nil), hs...), Bytes(HEndOfBody, body))
	req, err := c.request(OpPutFinal, all)
	if err != nil {
		return err
	}
	_, err = c.final(req)
	return err
}

// Disconnect sends Disconnect and closes the connection whatever the answer.
func (c *Client) Disconnect() error {
	req, err := c.request(OpDisconnect, nil)
	if err != nil {
		_ = c.c.Close()
		return err
	}
	_, err = c.final(req)
	_ = c.c.Close()
	return err
}

// Get sends hs in one final Get and repeats an empty final Get while the answer is
// Continue, returning the collected body. The answer is capped at MaxBody bytes of body and
// MaxHeaderBytes of other headers, and the whole operation at the operation deadline; over
// either, Abort is sent (waiting at most AbortWait for its answer) and the connection closed.
func (c *Client) Get(hs ...Header) ([]byte, error) {
	req, err := c.request(OpGetFinal, hs)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(operationTimeout)
	var body []byte
	hdrBytes := 0
	for {
		if err := c.c.Write(req); err != nil {
			return nil, c.fail(err)
		}
		wait, opLimited := packetTimeout, false
		if rem := time.Until(deadline); rem <= 0 {
			return nil, c.abort(fmt.Errorf("%w: operation deadline", ErrTimeout))
		} else if rem < wait {
			wait, opLimited = rem, true
		}
		b, err := c.c.Next(wait)
		if err != nil {
			if opLimited && err == ErrTimeout {
				return nil, c.abort(fmt.Errorf("%w: operation deadline", ErrTimeout))
			}
			return nil, c.fail(err)
		}
		p, err := ParseResponse(b, false)
		if err != nil {
			return nil, c.fail(err)
		}
		if p.Code&0x80 == 0 {
			return nil, c.fail(protocolf("invalid response code"))
		}
		if p.Code != RespContinue && p.Code != RespSuccess {
			return nil, &ResponseError{Code: p.Code}
		}
		for _, h := range p.Headers {
			if h.ID == HBody || h.ID == HEndOfBody {
				body = append(body, h.Value...)
			} else {
				hdrBytes += len(h.Value) + 3
			}
		}
		if len(body) > MaxBody || hdrBytes > MaxHeaderBytes {
			return nil, c.abort(fmt.Errorf("answer over the cap: %w", ErrTooLarge))
		}
		if p.Code == RespSuccess {
			if body == nil {
				body = []byte{}
			}
			return body, nil
		}
		if req, err = c.request(OpGetFinal, nil); err != nil {
			return nil, c.fail(err)
		}
	}
}

// abort sends Abort, waits briefly for its answer, closes and returns cause.
func (c *Client) abort(cause error) error {
	if req, err := c.request(OpAbort, nil); err == nil && c.c.Write(req) == nil {
		end := time.Now().Add(abortWait)
		for {
			rem := time.Until(end)
			if rem <= 0 {
				break
			}
			b, err := c.c.Next(rem)
			if err != nil {
				break
			}
			// A response already in flight may precede the answer to Abort.
			if len(b) > 0 && b[0] != RespContinue {
				break
			}
		}
	}
	return c.fail(cause)
}

// Done is closed when the connection is gone, an idle EOF included.
func (c *Client) Done() <-chan struct{} { return c.c.Done() }

// Close closes the transport.
func (c *Client) Close() error { return c.c.Close() }
