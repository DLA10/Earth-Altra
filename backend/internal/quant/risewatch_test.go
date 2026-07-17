package quant

import (
	"context"
	"testing"
	"time"

	"live-optimus/backend/internal/candles"
)

// riseTestZone returns a fixed zone where the local wall clock reads ~10:00 right now, so
// the session-cutoff guard (no arms/entries at/after 15:30) never trips during the test.
func riseTestZone() *time.Location {
	now := time.Now().UTC()
	offset := 10*3600 - (now.Hour()*3600 + now.Minute()*60 + now.Second())
	return time.FixedZone("risetest", offset)
}

// seedRise seeds 1-min bars: flat at price until armedAt, then the given closes after it
// (green when close > price, completed — each bar ends before now).
func seedRise(eng *candles.Engine, sym string, price float64, armedAt time.Time, closes []float64) {
	var bars []candles.Candle
	base := armedAt.Add(-20 * time.Minute).Unix()
	for i := 0; i < 20; i++ {
		bars = append(bars, candles.Candle{Time: base + int64(i*60), Open: price, High: price, Low: price, Close: price, Volume: 100})
	}
	for i, c := range closes {
		t := armedAt.Unix() + int64(i*60)
		hi := price
		if c > hi {
			hi = c
		}
		bars = append(bars, candles.Candle{Time: t, Open: price, High: hi, Low: price - 0.5, Close: c, Volume: 100})
	}
	eng.Seed(sym, bars)
}

func TestRiseWatchConfirmAndShadowTrigger(t *testing.T) {
	loc := riseTestZone()
	ceng := candles.NewEngine([]string{"BNC"}, 500)
	armedAt := time.Now().Add(-5 * time.Minute)
	// A completed green bar closing +0.2% above the dip price, dip low intact, volume
	// steady → confirmed (the month-replay-validated rule).
	seedRise(ceng, "BNC", 100, armedAt, []float64{100.05, 100.20})

	alloc := NewAllocator()
	alloc.Configure(Allocation{BudgetUSD: 4000, PerPositionUSD: 2000, MaxConcurrent: 2})
	e := NewEngine(nil, alloc, nil, nil, ceng, nil, loc)

	rw := NewRiseWatch(e, nil, nil, loc, false, nil) // shadow: no manager needed
	de := DipEvent{Symbol: "BNC", Price: 100, DipLow: 99, PreDipHigh: 102}
	if !rw.Arm(de, 0.55) {
		t.Fatal("Arm should accept a fresh dip before the session cutoff")
	}
	rw.mu.Lock()
	rw.pending["BNC"].armedAt = armedAt // backdate so the seeded bars fall inside the window
	rw.mu.Unlock()

	rw.scan(context.Background())

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) != 0 {
		t.Fatalf("confirmed rise should leave no pending arms, got %d", len(rw.pending))
	}
	if !rw.traded["BNC"] {
		t.Fatal("shadow trigger should mark the symbol as rise-traded for the day")
	}
}

func TestRiseWatchRedBarDoesNotConfirm(t *testing.T) {
	loc := riseTestZone()
	ceng := candles.NewEngine([]string{"RED"}, 500)
	armedAt := time.Now().Add(-5 * time.Minute)
	// Green bar, then a bar closing above the level but RED: must not confirm.
	seedRise(ceng, "RED", 100, armedAt, []float64{100.05, 100.20})
	// Overwrite: make the qualifying bar red by raising its open above its close.
	bars := ceng.Snapshot("RED", 1)
	bars[len(bars)-1].Open = 100.50
	ceng.Seed("RED", bars)

	e := NewEngine(nil, NewAllocator(), nil, nil, ceng, nil, loc)
	rw := NewRiseWatch(e, nil, nil, loc, false, nil)
	if !rw.Arm(DipEvent{Symbol: "RED", Price: 100, DipLow: 99}, 0.5) {
		t.Fatal("Arm should accept the dip")
	}
	rw.mu.Lock()
	rw.pending["RED"].armedAt = armedAt
	rw.mu.Unlock()

	rw.scan(context.Background())

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) != 1 {
		t.Fatalf("a red bar above the level must not confirm; pending=%d", len(rw.pending))
	}
	if rw.traded["RED"] {
		t.Fatal("nothing should have triggered")
	}
}

func TestRiseWatchDipLowUndercutDisarms(t *testing.T) {
	loc := riseTestZone()
	ceng := candles.NewEngine([]string{"CUT"}, 500)
	armedAt := time.Now().Add(-5 * time.Minute)
	// First bar undercuts the dip low, then a green bar clears the level: the undercut
	// killed the bounce thesis, so the arm must be dropped WITHOUT triggering.
	seedRise(ceng, "CUT", 100, armedAt, []float64{99.95, 100.20})
	bars := ceng.Snapshot("CUT", 1)
	bars[len(bars)-2].Low = 98.90 // below the 99 dip low
	ceng.Seed("CUT", bars)

	e := NewEngine(nil, NewAllocator(), nil, nil, ceng, nil, loc)
	rw := NewRiseWatch(e, nil, nil, loc, false, nil)
	if !rw.Arm(DipEvent{Symbol: "CUT", Price: 100, DipLow: 99}, 0.5) {
		t.Fatal("Arm should accept the dip")
	}
	rw.mu.Lock()
	rw.pending["CUT"].armedAt = armedAt
	rw.mu.Unlock()

	rw.scan(context.Background())

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) != 0 {
		t.Fatal("an undercut dip low must disarm the watch")
	}
	if rw.traded["CUT"] {
		t.Fatal("an undercut dip must never trigger an entry")
	}
}

func TestRiseWatchVolumeFadeDoesNotConfirm(t *testing.T) {
	loc := riseTestZone()
	ceng := candles.NewEngine([]string{"FAD"}, 500)
	armedAt := time.Now().Add(-5 * time.Minute)
	// The qualifying green bar's volume is far below the post-dip average: no trigger.
	seedRise(ceng, "FAD", 100, armedAt, []float64{99.95, 100.20})
	bars := ceng.Snapshot("FAD", 1)
	bars[len(bars)-2].Volume = 5000 // heavy first post-dip bar
	bars[len(bars)-1].Volume = 100  // fading confirmation bar
	ceng.Seed("FAD", bars)

	e := NewEngine(nil, NewAllocator(), nil, nil, ceng, nil, loc)
	rw := NewRiseWatch(e, nil, nil, loc, false, nil)
	if !rw.Arm(DipEvent{Symbol: "FAD", Price: 100, DipLow: 99}, 0.5) {
		t.Fatal("Arm should accept the dip")
	}
	rw.mu.Lock()
	rw.pending["FAD"].armedAt = armedAt
	rw.mu.Unlock()

	rw.scan(context.Background())

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) != 1 || rw.traded["FAD"] {
		t.Fatal("a fading-volume confirmation must not trigger")
	}
}

func TestRiseWatchExpiry(t *testing.T) {
	loc := riseTestZone()
	ceng := candles.NewEngine([]string{"EXP"}, 500)
	armedAt := time.Now().Add(-30 * time.Minute) // past the 20-minute window
	seedRise(ceng, "EXP", 100, armedAt, []float64{99.9})

	e := NewEngine(nil, NewAllocator(), nil, nil, ceng, nil, loc)
	rw := NewRiseWatch(e, nil, nil, loc, false, nil)
	if !rw.Arm(DipEvent{Symbol: "EXP", Price: 100, DipLow: 99}, 0.5) {
		t.Fatal("Arm should accept the dip")
	}
	rw.mu.Lock()
	rw.pending["EXP"].armedAt = armedAt
	rw.mu.Unlock()

	rw.scan(context.Background())

	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) != 0 {
		t.Fatal("an arm past its window must expire")
	}
	if rw.traded["EXP"] {
		t.Fatal("an expired arm must not count as traded")
	}
}
