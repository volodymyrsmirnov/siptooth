package sdp

import (
	"bytes"
	"fmt"
	"slices"
)

// ReofferKind is the verdict on a re-INVITE (or the SDP in its ACK).
type ReofferKind int

const (
	Refresh       ReofferKind = iota // acceptable, nothing changed
	AddressChange                    // acceptable, remote address or port changed
	Reject                           // answer 488, existing media continues
)

// ClassifyReoffer applies spec section 5.6 to a re-offer against the
// established media. The returned Media is current with Addr updated for
// AddressChange (and current unchanged otherwise). An empty body is an error
// (the caller handles offerless re-INVITEs); every other refusal is a Reject
// with a nil error.
func ClassifyReoffer(body []byte, current Media, _ Offer) (ReofferKind, Media, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Reject, current, fmt.Errorf("sdp: empty re-offer body")
	}
	st, err := parseStream(body)
	if err != nil {
		return Reject, current, nil
	}
	key, err := st.checkSecurity(current.SRTP)
	if err != nil {
		return Reject, current, nil
	}
	if current.SRTP && !bytes.Equal(key, current.RemoteKey) {
		return Reject, current, nil
	}
	if !slices.Contains(st.pts, int(current.SendCodec)) {
		return Reject, current, nil
	}
	if st.addr == current.Addr {
		return Refresh, current, nil
	}
	next := current
	next.Addr = st.addr
	return AddressChange, next, nil
}
