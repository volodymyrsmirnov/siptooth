package g711

import "testing"

func TestAlawKnownVectors(t *testing.T) {
	cases := []struct {
		pcm  int16
		code byte
	}{{0, 0xD5}, {-1, 0x55}, {32767, 0xAA}, {-32768, 0x2A}}
	for _, c := range cases {
		if got := AlawEncode([]int16{c.pcm})[0]; got != c.code {
			t.Errorf("encode(%d) = %#x, want %#x", c.pcm, got, c.code)
		}
	}
	if v := AlawDecode([]byte{0xD5})[0]; v != 8 {
		t.Errorf("decode(0xD5) = %d, want 8", v)
	}
	if v := AlawDecode([]byte{0x55})[0]; v != -8 {
		t.Errorf("decode(0x55) = %d, want -8", v)
	}
}

// step returns the A-law quantization step (16-bit scale) for a magnitude.
func alawStep(mag int) int {
	m := mag >> 3
	step := 16
	for end := 0x3F; m > end && step < 1024; end = end<<1 | 1 {
		step <<= 1
	}
	return step
}

func TestAlawRoundTripWithinStep(t *testing.T) {
	for x := -32768; x <= 32767; x++ {
		back := int(AlawDecode(AlawEncode([]int16{int16(x)}))[0])
		mag := x
		if mag < 0 {
			mag = -mag
		}
		if d := x - back; d > alawStep(mag) || -d > alawStep(mag) {
			t.Fatalf("%d round-tripped to %d (step %d)", x, back, alawStep(mag))
		}
	}
}

func TestAlawDecodeEncodeStable(t *testing.T) {
	for c := 0; c < 256; c++ {
		v := AlawDecode([]byte{byte(c)})
		if got := AlawEncode(v)[0]; got != byte(c) {
			t.Errorf("code %#x -> %d -> %#x", c, v[0], got)
		}
	}
}

func TestSilenceByte(t *testing.T) {
	for _, c := range []Codec{PCMU, PCMA} {
		if v := c.Decode([]byte{c.SilenceByte()})[0]; v > 8 || v < -8 {
			t.Errorf("%v silence decodes to %d", c, v)
		}
	}
}

func TestParseCodec(t *testing.T) {
	if c, err := ParseCodec("pcma"); err != nil || c != PCMA || c.String() != "PCMA" {
		t.Errorf("pcma: %v %v", c, err)
	}
	if c, err := ParseCodec("PCMU"); err != nil || c != PCMU {
		t.Errorf("PCMU: %v %v", c, err)
	}
	if _, err := ParseCodec("opus"); err == nil {
		t.Error("want error for opus")
	}
}

func TestCodecDispatch(t *testing.T) {
	pcm := []int16{0, 1000, -1000}
	if string(PCMA.Encode(pcm)) != string(AlawEncode(pcm)) || string(PCMU.Encode(pcm)) != string(MulawEncode(pcm)) {
		t.Error("Encode dispatch mismatch")
	}
	if len(PCMA.Decode([]byte{1, 2})) != 2 {
		t.Error("Decode length")
	}
}
