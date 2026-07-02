package quant

import "testing"

func TestMinConfidenceFloor(t *testing.T) {
	a := NewAllocator()
	a.Configure(Allocation{BudgetUSD: 6000, PerPositionUSD: 2000, MaxConcurrent: 3})
	if got := a.Size(0.49); got != 0 {
		t.Fatalf("conf 0.49 should fund $0 (below 0.5 floor), got %v", got)
	}
	if got := a.Size(0.5); got != 1000 {
		t.Fatalf("conf 0.5 should fund a half slice ($1000), got %v", got)
	}
	if got := a.Size(0.7); got != 2000 {
		t.Fatalf("conf 0.7 should fund a full slice ($2000), got %v", got)
	}
}

func TestAttributionVerdict(t *testing.T) {
	// Only-discretionary exits → no valid baseline → verdict must be false (insufficient data).
	att := attribution([]QuantTrade{
		{ExitReason: "AI_Exit", PNL: 50},
		{ExitReason: "Take_Profit", PNL: 30},
	})
	if att.Agent3AddsValue {
		t.Fatal("verdict should be false with no stop exits to compare against")
	}
	// Both kinds present, discretionary avg beats stop avg → adds value.
	att = attribution([]QuantTrade{
		{ExitReason: "AI_Exit", PNL: 60},
		{ExitReason: "Trail_Stop", PNL: -20},
	})
	if !att.Agent3AddsValue {
		t.Fatalf("verdict should be true: discretionary +60 vs stop -20 (got disc=%v stop=%v)", att.DiscretionaryAvg, att.StopAvg)
	}
}
