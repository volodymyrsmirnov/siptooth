package rtp

import (
	"fmt"

	"github.com/pion/srtp/v3"
)

const srtpKeyLen = 30 // 16-byte master key || 14-byte master salt

func newSendContext(key []byte) (*srtp.Context, error) {
	if len(key) != srtpKeyLen {
		return nil, fmt.Errorf("rtp: srtp send key must be %d bytes, got %d", srtpKeyLen, len(key))
	}
	return srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAes128CmHmacSha1_80)
}

func newRecvContext(key []byte) (*srtp.Context, error) {
	if len(key) != srtpKeyLen {
		return nil, fmt.Errorf("rtp: srtp receive key must be %d bytes, got %d", srtpKeyLen, len(key))
	}
	return srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAes128CmHmacSha1_80,
		srtp.SRTPReplayProtection(64))
}
