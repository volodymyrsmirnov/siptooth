package g711

import (
	"math"
	"testing"
)

func TestMulawKnownValues(t *testing.T) {
	cases := []struct {
		pcm  int16
		code byte
	}{
		{0, 0xFF}, {-1, 0x7F}, {32767, 0x80}, {-32768, 0x00}, {1000, 0xCE}, {-1000, 0x4E},
	}
	for _, c := range cases {
		if got := MulawEncode([]int16{c.pcm})[0]; got != c.code {
			t.Errorf("encode(%d) = %#x, want %#x", c.pcm, got, c.code)
		}
	}
	if MulawDecode([]byte{0xFF})[0] != 0 {
		t.Error("0xFF must decode to silence")
	}
	if v := MulawDecode([]byte{0x80})[0]; v < 31000 {
		t.Errorf("0x80 decoded to %d", v)
	}
}

func TestMulawRoundTripError(t *testing.T) {
	for _, s := range []int16{0, 7, -7, 100, -100, 1234, -1234, 10000, -10000, 30000, -30000} {
		back := MulawDecode(MulawEncode([]int16{s}))[0]
		diff := math.Abs(float64(back) - float64(s))
		tol := math.Max(8, math.Abs(float64(s))*0.07) // μ-law is logarithmic: ~7% relative error
		if diff > tol {
			t.Errorf("sample %d round-tripped to %d (diff %.0f > %.0f)", s, back, diff, tol)
		}
	}
}

func TestMulawSymmetry(t *testing.T) {
	for _, s := range []int16{10, 500, 3000, 20000} {
		plus := MulawDecode(MulawEncode([]int16{s}))[0]
		minus := MulawDecode(MulawEncode([]int16{-s}))[0]
		if plus != -minus {
			t.Errorf("asymmetric: %d vs %d", plus, minus)
		}
	}
}

func TestMulawLengths(t *testing.T) {
	if len(MulawEncode(make([]int16, 32))) != 32 || len(MulawDecode(make([]byte, 160))) != 160 {
		t.Fatal("length mismatch")
	}
}
