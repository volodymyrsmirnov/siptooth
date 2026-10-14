package g711

// G.711 A-law per ITU-T (same as the Sun/CCITT reference code): 13-bit magnitude, XOR 0x55.

const AlawSilence byte = 0xD5

var alawSegEnd = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

func alawEncodeSample(s int16) byte {
	v := int(s) >> 3 // 16-bit to 13-bit, arithmetic shift
	mask := 0xD5
	if v < 0 {
		mask = 0x55
		v = -v - 1
	}
	seg := 8
	for i, end := range alawSegEnd {
		if v <= end {
			seg = i
			break
		}
	}
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	aval := seg << 4
	if seg < 2 {
		aval |= (v >> 1) & 0x0F
	} else {
		aval |= (v >> seg) & 0x0F
	}
	return byte(aval ^ mask)
}

func alawDecodeSample(b byte) int16 {
	a := int(b) ^ 0x55
	t := (a & 0x0F) << 4
	seg := (a & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

func AlawEncode(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = alawEncodeSample(s)
	}
	return out
}

func AlawDecode(b []byte) []int16 {
	out := make([]int16, len(b))
	for i, c := range b {
		out[i] = alawDecodeSample(c)
	}
	return out
}
