package signals

import (
	"math"
	"testing"
)

// TestRidgeRecoversLinearSignal: rows where r_multiple = 0.5*rvol - 0.4*market_down +
// noiseless constant must yield a model that ranks high-rvol/market-up signals above
// low-rvol/market-down ones and predicts near the true values.
func TestRidgeRecoversLinearSignal(t *testing.T) {
	var rows []DatasetRow
	// Deterministic grid — no randomness so the test is stable.
	for i := 0; i < 200; i++ {
		rvol := 0.5 + float64(i%10)*0.3   // 0.5..3.2
		mdown := float64((i / 10) % 2)    // 0 or 1
		r := 0.5*rvol - 0.8*mdown - 0.45  // true relationship
		rows = append(rows, DatasetRow{
			Strategy:  "test",
			RMultiple: r,
			Features:  map[string]float64{"rvol": rvol, "market_down": mdown},
		})
	}
	g := NewGate()
	g.MinRows = 50
	g.Train(rows)
	m := g.Models()["test"]
	if m == nil {
		t.Fatal("model not trained")
	}

	good := Signal{Strategy: "test", Features: map[string]float64{"rvol": 3.0, "market_down": 0}}
	bad := Signal{Strategy: "test", Features: map[string]float64{"rvol": 0.6, "market_down": 1}}
	okG, predG, appliedG := g.Allow(good)
	okB, predB, appliedB := g.Allow(bad)
	if !appliedG || !appliedB {
		t.Fatal("gate should be applied for the trained strategy")
	}
	if !okG {
		t.Fatalf("high-quality signal rejected (pred %.3f)", predG)
	}
	if okB {
		t.Fatalf("low-quality signal accepted (pred %.3f)", predB)
	}
	wantG := 0.5*3.0 - 0.45
	if math.Abs(predG-wantG) > 0.1 {
		t.Fatalf("prediction off: got %.3f want ~%.2f", predG, wantG)
	}

	// Unknown strategy → warmup pass-through signal.
	if _, _, applied := g.Allow(Signal{Strategy: "other"}); applied {
		t.Fatal("untrained strategy must report applied=false")
	}
}
