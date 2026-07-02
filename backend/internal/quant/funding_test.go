package quant

import (
	"sync"
	"testing"
	"time"

	"live-optimus/backend/internal/candles"
)

// seedFlat seeds a symbol with flat 1-min bars at the given price so LastClose returns it.
func seedFlat(eng *candles.Engine, sym string, price float64) {
	base := time.Date(2026, 6, 26, 13, 30, 0, 0, time.UTC).Unix() // a regular-session minute
	bars := make([]candles.Candle, 0, 21)
	for i := 0; i < 21; i++ {
		t := base + int64(i*60)
		bars = append(bars, candles.Candle{Time: t, Open: price, High: price, Low: price, Close: price, Volume: 100})
	}
	eng.Seed(sym, bars)
}

// TestFundContendedRanksBestFirst verifies that when two approved buys contend for a single slot,
// the funding coordinator funds the higher dip-quality one and skips the weaker one.
func TestFundContendedRanksBestFirst(t *testing.T) {
	eng := candles.NewEngine([]string{"STRONG", "WEAK"}, 200)
	seedFlat(eng, "STRONG", 10)
	seedFlat(eng, "WEAK", 10)

	alloc := NewAllocator()
	alloc.Configure(Allocation{BudgetUSD: 2000, PerPositionUSD: 2000, MaxConcurrent: 1}) // room for ONE

	e := NewEngine(nil, alloc, nil, nil, eng, nil, time.UTC)
	e.SetLive(true)

	strong := Candidate{Symbol: "STRONG", Confidence: 0.7, Tier: 1, Dip: DipEvent{RVOL: 2.2, DepthATR: 1.5}}
	weak := Candidate{Symbol: "WEAK", Confidence: 0.7, Tier: 2, Dip: DipEvent{RVOL: 1.0, DepthATR: 0.5}}

	var wg sync.WaitGroup
	results := map[string]float64{}
	var mu sync.Mutex
	for _, c := range []Candidate{weak, strong} { // submit weak FIRST to prove it's not first-come
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			size, _ := e.fundContended(c)
			mu.Lock()
			results[c.Symbol] = size
			mu.Unlock()
		}()
		time.Sleep(50 * time.Millisecond) // ensure WEAK enters the round before STRONG
	}
	wg.Wait()

	if results["STRONG"] != 2000 {
		t.Fatalf("STRONG should be funded $2000 first, got %v", results["STRONG"])
	}
	if results["WEAK"] != 0 {
		t.Fatalf("WEAK should be skipped (slot taken by STRONG), got %v", results["WEAK"])
	}
	if alloc.OpenCount() != 1 || !alloc.Held("STRONG") {
		t.Fatalf("allocator should hold only STRONG (open=%d)", alloc.OpenCount())
	}
}
