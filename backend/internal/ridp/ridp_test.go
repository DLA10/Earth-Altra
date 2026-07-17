package ridp

import (
	"testing"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

// mkBars builds n daily bars ending YESTERDAY (ET) with the given closes; highs/lows
// bracket the close, volume constant. Day strings ascend.
func mkBars(closes []float64) []DailyBar {
	et, _ := time.LoadLocation("America/New_York")
	out := make([]DailyBar, len(closes))
	day := time.Now().In(et).AddDate(0, 0, -len(closes)-1) // last bar lands on YESTERDAY
	for i, c := range closes {
		day = day.AddDate(0, 0, 1)
		out[i] = DailyBar{Day: day.Format("2006-01-02"), Open: c, High: c * 1.01, Low: c * 0.99, Close: c, Volume: 1e6}
	}
	return out
}

// TestVolumeProfileTimeOfDay proves the RVOL fix: a front-loaded (U-shaped) volume day must
// yield a cumulative curve that is ABOVE the old flat linear ramp in the morning — otherwise
// RIDER's "2x normal for this time of day" gate is measuring against the wrong baseline.
func TestVolumeProfileTimeOfDay(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	// Seven consecutive June 2026 days (>=5 weekdays) with heavy open/close, quiet midday.
	var bars []VolBar
	for d := 0; d < 7; d++ {
		day := time.Date(2026, 6, 1+d, 9, 30, 0, 0, et)
		for m := 0; m < sessionMinutes; m++ {
			vol := 100.0
			if m < 30 || m >= sessionMinutes-30 { // first & last 30 min carry the load
				vol = 500.0
			}
			bars = append(bars, VolBar{Time: day.Add(time.Duration(m) * time.Minute).Unix(), Volume: vol})
		}
	}
	prof := BuildVolumeProfile(et, bars)
	if prof == nil {
		t.Fatal("expected a profile from 7 days of bars")
	}
	// At minute 60 (10:30 ET, RIDER's first entry) far more than the linear 60/390 ≈ 15.4%
	// of the day's volume has already traded — the whole point of the fix.
	linear := 60.0 / float64(sessionMinutes)
	if prof[60] <= linear {
		t.Errorf("front-loaded volume: expected prof[60]=%.3f > linear %.3f", prof[60], linear)
	}
	// Monotonic non-decreasing, terminating at ~1.0.
	for i := 1; i < len(prof); i++ {
		if prof[i] < prof[i-1] {
			t.Fatalf("profile must be non-decreasing (broke at minute %d)", i)
		}
	}
	if prof[sessionMinutes-1] < 0.99 {
		t.Errorf("cumulative fraction should reach ~1.0, got %.3f", prof[sessionMinutes-1])
	}
	// A manager with this profile installed must report the curve fraction, not the linear one.
	m := newTestMgr(t, map[string][]DailyBar{})
	m.SetVolumeProfiles(map[string][]float64{"AAA": prof})
	if got := m.expectedVolFrac("AAA", 60); got <= linear {
		t.Errorf("expectedVolFrac should use the profile: got %.3f, linear %.3f", got, linear)
	}
	// Unknown symbol falls back to the linear estimate.
	if got := m.expectedVolFrac("ZZZ", 60); got != linear {
		t.Errorf("unknown symbol should fall back to linear %.3f, got %.3f", linear, got)
	}
}

// TestReverterZScore locks the REVERTER entry/exit math: a price stretched below its window
// mean reads a negative z (a buy candidate at <=-1.5), a price back at the mean reads ~0 (exit),
// and a flat window is rejected (no tradeable band).
func TestReverterZScore(t *testing.T) {
	// window of 149..151 oscillation, last bar dips to 149 => stretched well below the ~150 mean
	dip := []float64{150, 151, 150, 149.5, 150.5, 151, 150, 149, 150, 151, 150.5, 149.5, 150, 151, 148.8}
	z, std, ok := zscore(dip)
	if !ok {
		t.Fatal("expected a valid z on an oscillating window")
	}
	if std <= 0 {
		t.Error("std (band width) must be positive on an oscillating window")
	}
	if z > reverterZIn {
		t.Errorf("a dip to the band floor should trigger entry: z=%.2f must be <= %.2f", z, reverterZIn)
	}
	// price sitting at the mean => |z| small, should NOT be an entry and should satisfy exit
	flatAtMean := append(append([]float64{}, dip[:14]...), 150.0)
	z2, _, ok2 := zscore(flatAtMean)
	if !ok2 {
		t.Fatal("expected valid z")
	}
	if z2 <= reverterZIn {
		t.Errorf("price at the mean must not be an entry: z=%.2f", z2)
	}
	// a perfectly flat window has no band => rejected
	if _, _, ok3 := zscore([]float64{100, 100, 100, 100, 100}); ok3 {
		t.Error("a flat window must be rejected (no std => no band)")
	}
}

func newTestMgr(t *testing.T, bars map[string][]DailyBar) *Manager {
	t.Helper()
	et, _ := time.LoadLocation("America/New_York")
	eng := candles.NewEngine([]string{"TST"}, 100)
	m := New(quant.NewBroker("", "", ""), eng, []string{"TST"}, et, t.TempDir(), false,
		func([]string, int) (map[string][]DailyBar, error) { return bars, nil })
	m.refreshDaily()
	return m
}

// TestDipperTrigger: 4 red closes then a day that CLOSES above the prior day's high must
// mark the symbol Triggered (buy at next open).
func TestDipperTrigger(t *testing.T) {
	closes := []float64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116,
		115, 113, 111, 109, // 4 consecutive red closes = setup
		112.5,              // closes above prior high (109*1.01=110.09) = the turn
	}
	m := newTestMgr(t, map[string][]DailyBar{"TST": mkBars(closes)})
	d := m.daily["TST"]
	if d == nil {
		t.Fatal("daily ctx missing")
	}
	if !d.Setup {
		t.Error("expected a qualified falling setup (4 red closes)")
	}
	if !d.Triggered {
		t.Errorf("expected the turn to be Triggered (close 112.5 > prior high %.2f)", 109*1.01)
	}
	if d.ATR <= 0 {
		t.Error("ATR should be positive")
	}
}

// TestDipperNoTriggerWithoutSetup: the same turn-shaped close WITHOUT a preceding fall
// must not trigger — DIPPER only buys turns out of real dips.
func TestDipperNoTriggerWithoutSetup(t *testing.T) {
	closes := []float64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119, 120,
		123, // strong up close, but no setup before it
	}
	m := newTestMgr(t, map[string][]DailyBar{"TST": mkBars(closes)})
	d := m.daily["TST"]
	if d == nil {
		t.Fatal("daily ctx missing")
	}
	if d.Setup || d.Triggered {
		t.Errorf("no setup expected on an uptrend (setup=%v triggered=%v)", d.Setup, d.Triggered)
	}
}
