package sms

import (
	"fmt"
	"io"

	"github.com/volodymyrsmirnov/siptooth/internal/obex"
)

const typeEventReport = "x-bt/MAP-event-report"

// ServeMNS answers one MNS connection from the phone (spec 17.6, "MNS event validation") and
// closes rwc when it returns. onConnected (may be nil) is called once after the OBEX Connect
// succeeded. onEvents (may be nil) is called for each event report Put with the MAS instance
// ID (-1 when absent), the parsed events, or the parse error (the phone still gets Success
// so it does not retry). A Put of another Type is answered Bad Request. It returns nil
// after a Disconnect and otherwise the reason the connection ended.
func ServeMNS(rwc io.ReadWriteCloser, onConnected func(), onEvents func(instance int, evs []Event, err error)) error {
	h := func(hs obex.Headers, body []byte) byte {
		t, ok := hs.Get(obex.HType)
		if !ok || obex.DecodeASCII(t) != typeEventReport {
			return obex.RespBadRequest
		}
		instance := -1
		var perr error
		if v, ok := hs.Get(obex.HAppParams); ok {
			ps, err := obex.DecodeAppParams(v)
			if err != nil {
				perr = fmt.Errorf("%w: application parameters", ErrMalformed)
			}
			for _, p := range ps {
				if p.Tag == tagMASInstanceID && len(p.Value) == 1 {
					instance = int(p.Value[0])
				}
			}
		}
		var evs []Event
		if perr == nil {
			evs, perr = ParseEventReport(body)
		}
		if onEvents != nil {
			onEvents(instance, evs, perr)
		}
		return obex.RespSuccess
	}
	return obex.Serve(rwc, MNSTarget, h, onConnected)
}
