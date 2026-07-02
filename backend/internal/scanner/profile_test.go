package scanner

import (
	"math"
	"testing"
	"time"
)

func TestVolumeProfile(t *testing.T) {
	scn := New(map[string]string{"X": "test"})
	mkDay := func(d int, volAt func(m int) float64) []Bar {
		start := time.Date(2026, 6, d, 9, 30, 0, 0, scn.et).Unix()
		var bars []Bar
		for m := 0; m < 390; m++ {
			bars = append(bars, Bar{Time: start + int64(m*60), Volume: volAt(m), Close: 100, VWAP: 100})
		}
		return bars
	}
	// Two UNIFORM-volume weekday sessions (Jun 23 Tue, Jun 24 Wed) → cumFrac[m] = (m+1)/390.
	var bars []Bar
	bars = append(bars, mkDay(23, func(int) float64 { return 1000 })...)
	bars = append(bars, mkDay(24, func(int) float64 { return 1000 })...)
	prof := scn.BuildVolumeProfile(bars)
	if len(prof) != 390 {
		t.Fatalf("profile len = %d, want 390", len(prof))
	}
	for _, m := range []int{0, 100, 200, 389} {
		want := float64(m+1) / 390
		if math.Abs(prof[m]-want) > 1e-6 {
			t.Fatalf("uniform frac[%d]=%.6f, want %.6f", m, prof[m], want)
		}
	}
	// FRONT-LOADED day: all volume in the first minute → cumFrac == 1.0 from minute 0 on.
	fp := scn.BuildVolumeProfile(mkDay(24, func(m int) float64 {
		if m == 0 {
			return 5000
		}
		return 0
	}))
	for _, m := range []int{0, 50, 389} {
		if math.Abs(fp[m]-1.0) > 1e-9 {
			t.Fatalf("front-loaded frac[%d]=%.6f, want 1.0", m, fp[m])
		}
	}
	t.Logf("OK: uniform-day curve is linear, front-loaded-day curve is ~1.0 throughout")
}
