// Package g711 implements the ITU-T G.711 PCMU (mu-law) and PCMA (A-law) codecs.
package g711

import (
	"fmt"
	"strings"
)

// Codec is a G.711 variant; its value is the static RTP payload type.
type Codec int

const (
	PCMU Codec = 0
	PCMA Codec = 8
)

func (c Codec) String() string {
	switch c {
	case PCMU:
		return "PCMU"
	case PCMA:
		return "PCMA"
	}
	return fmt.Sprintf("Codec(%d)", int(c))
}

// ParseCodec accepts "PCMU" or "PCMA", case-insensitively.
func ParseCodec(name string) (Codec, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "PCMU":
		return PCMU, nil
	case "PCMA":
		return PCMA, nil
	}
	return 0, fmt.Errorf("unknown codec %q (want PCMU or PCMA)", name)
}

func (c Codec) Encode(pcm []int16) []byte {
	if c == PCMA {
		return AlawEncode(pcm)
	}
	return MulawEncode(pcm)
}

func (c Codec) Decode(b []byte) []int16 {
	if c == PCMA {
		return AlawDecode(b)
	}
	return MulawDecode(b)
}

// SilenceByte is the encoded value of digital silence.
func (c Codec) SilenceByte() byte {
	if c == PCMA {
		return AlawSilence
	}
	return MulawSilence
}
