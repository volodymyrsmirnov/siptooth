package rtp

import (
	"math"
	"testing"
)

func TestLevelsSpeechPercentiles(t *testing.T) {
	var l levels
	frame := func(amp float64) []int16 {
		f := make([]int16, 160)
		for i := range f {
			f[i] = int16(amp * 32767 * math.Sin(2*math.Pi*440*float64(i)/8000))
		}
		return f
	}
	for i := 0; i < 50; i++ {
		l.add(make([]int16, 160)) // silence: not speech
	}
	for i := 0; i < 90; i++ {
		l.add(frame(0.1)) // -23 dBFS RMS
	}
	for i := 0; i < 10; i++ {
		l.add(frame(0.5)) // -9 dBFS RMS
	}
	p50, p95, speech := l.summary()
	if p50 != -23 {
		t.Fatalf("p50 = %d dBFS, want -23", p50)
	}
	if p95 != -9 {
		t.Fatalf("p95 = %d dBFS, want -9", p95)
	}
	if speech != 100 {
		t.Fatalf("speech frames = %d, want 100", speech)
	}
}

func TestLevelsEmpty(t *testing.T) {
	var l levels
	if p50, p95, n := l.summary(); p50 != 0 || p95 != 0 || n != 0 {
		t.Fatalf("got %d %d %d", p50, p95, n)
	}
}
