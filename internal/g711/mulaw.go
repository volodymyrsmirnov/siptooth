package g711

// G.711 μ-law per ITU-T, with the usual 0x84 bias and 32635 clip (same as Sun/CCITT reference code).

const MulawSilence byte = 0xFF

const (
	mulawBias = 0x84
	mulawClip = 32635
)

func mulawEncodeSample(s int16) byte {
	v := int(s)
	sign := 0
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > mulawClip {
		v = mulawClip
	}
	v += mulawBias
	exp := 7
	for mask := 0x4000; v&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mant := (v >> (exp + 3)) & 0x0F
	return ^byte(sign | exp<<4 | mant)
}

func mulawDecodeSample(b byte) int16 {
	b = ^b
	sign := b & 0x80
	exp := int(b>>4) & 0x07
	mant := int(b & 0x0F)
	v := ((mant << 3) + mulawBias) << exp
	v -= mulawBias
	if sign != 0 {
		return int16(-v)
	}
	return int16(v)
}

func MulawEncode(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = mulawEncodeSample(s)
	}
	return out
}

func MulawDecode(b []byte) []int16 {
	out := make([]int16, len(b))
	for i, c := range b {
		out[i] = mulawDecodeSample(c)
	}
	return out
}
