// Package sdp builds siptooth's SDP offer and answer and validates the far
// end's SDP (spec sections 5.6, 6.4, 6.5). It is a small line-based parser for
// one audio stream; it is not a general SDP library.
package sdp

import (
	"errors"
	"net/netip"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
)

// ErrUnacceptable wraps every reason an answer or re-offer is not usable.
var ErrUnacceptable = errors.New("sdp: unacceptable")

// Media is the negotiated audio stream.
type Media struct {
	Addr      netip.AddrPort // remote RTP address
	SendCodec g711.Codec     // codec we transmit
	Recv      []g711.Codec   // payload types we accept (all negotiated)
	SRTP      bool
	RemoteKey []byte // 30 bytes key||salt from the answer (nil without SRTP)
}

// Offer describes what we advertise.
type Offer struct {
	SessID, Version uint64
	Addr            netip.AddrPort // advertised media address
	Codecs          []g711.Codec   // offer order
	LocalKey        []byte         // 30 bytes or nil (nil means plain RTP/AVP)
}

const (
	profileAVP  = "RTP/AVP"
	profileSAVP = "RTP/SAVP"
	cryptoSuite = "AES_CM_128_HMAC_SHA1_80"
	keyLen      = 30
)
