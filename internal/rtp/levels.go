package rtp

import "math"

// speechFloorRMS is the frame RMS (16-bit scale, about -56 dBFS) below which a
// frame is not counted as speech.
const speechFloorRMS = 50

// levels is a histogram of the RMS level (whole dBFS, -96..0) of the frames
// above the speech floor, for the caller level in the call summary.
type levels struct {
	bins [97]uint64 // index = -dBFS
	n    uint64
}

func (l *levels) add(pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	if rms < speechFloorRMS {
		return
	}
	db := int(math.Round(-20 * math.Log10(rms/32768)))
	l.bins[min(max(db, 0), 96)]++
	l.n++
}

// summary returns the median and 95th percentile speech level in dBFS and the
// number of speech frames; zeros without any.
func (l *levels) summary() (p50, p95 int, frames uint64) {
	if l.n == 0 {
		return 0, 0, 0
	}
	// Bins run from loud (0 dBFS) to quiet; a percentile of loudness counts
	// from the quiet end.
	pct := func(q float64) int {
		want := uint64(math.Ceil(q * float64(l.n)))
		var acc uint64
		for i := 96; i >= 0; i-- {
			acc += l.bins[i]
			if acc >= want {
				return -i
			}
		}
		return 0
	}
	return pct(0.5), pct(0.95), l.n
}
