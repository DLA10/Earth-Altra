package quant

import "testing"

func cfg() *Allocator {
	a := NewAllocator()
	a.Configure(Allocation{BudgetUSD: 5000, PerPositionUSD: 2000, MaxConcurrent: 2})
	return a
}

func TestSizingTiers(t *testing.T) {
	a := cfg()
	if got := a.Size(0.8); got != 2000 { // high conviction → full slice
		t.Fatalf("high-conviction size = %v, want 2000", got)
	}
	if got := a.Size(0.55); got != 1000 { // medium → half slice
		t.Fatalf("medium-conviction size = %v, want 1000", got)
	}
}

func TestMaxConcurrentAndBudget(t *testing.T) {
	a := cfg() // budget 5000, max 2 positions
	if !a.Fund("AAA", 2000) {
		t.Fatal("AAA should fund")
	}
	if !a.Fund("BBB", 2000) {
		t.Fatal("BBB should fund")
	}
	// Third position blocked by max-concurrent cap, even though $1000 is free.
	if a.CanFund("CCC") {
		t.Fatal("CCC should be blocked by max-concurrent cap")
	}
	if a.Fund("CCC", 1000) {
		t.Fatal("CCC fund should fail (cap)")
	}
	if a.OpenCount() != 2 {
		t.Fatalf("open=%d want 2", a.OpenCount())
	}
}

func TestCapitalRecycles(t *testing.T) {
	a := cfg()
	a.Fund("AAA", 2000)
	a.Fund("BBB", 2000)
	if a.Free() != 1000 {
		t.Fatalf("free=%v want 1000", a.Free())
	}
	a.Release("AAA") // position closes → capital returns
	if a.Free() != 3000 {
		t.Fatalf("after release free=%v want 3000", a.Free())
	}
	if !a.CanFund("CCC") { // a slot + capital freed up
		t.Fatal("CCC should now be fundable")
	}
}

func TestNoDoubleFundSameSymbol(t *testing.T) {
	a := cfg()
	a.Fund("AAA", 2000)
	if a.CanFund("AAA") || a.Fund("AAA", 2000) {
		t.Fatal("should not fund the same symbol twice")
	}
}

func TestRankingUnderContention(t *testing.T) {
	// Two approved buys; the higher-quality one (more RVOL + deeper dip + tier 1) ranks first.
	weak := Candidate{Symbol: "WEAK", Confidence: 0.7, Tier: 2, Dip: DipEvent{RVOL: 1.0, DepthATR: 0.5}}
	strong := Candidate{Symbol: "STRONG", Confidence: 0.7, Tier: 1, Dip: DipEvent{RVOL: 2.2, DepthATR: 1.5}}
	ranked := Rank([]Candidate{weak, strong})
	if ranked[0].Symbol != "STRONG" {
		t.Fatalf("expected STRONG first, got %s", ranked[0].Symbol)
	}
}

func TestSizeZeroWhenInsufficient(t *testing.T) {
	a := NewAllocator()
	a.Configure(Allocation{BudgetUSD: 2000, PerPositionUSD: 2000, MaxConcurrent: 3})
	a.Fund("AAA", 1500) // 500 free, less than a half-slice (1000)
	if got := a.Size(0.9); got != 0 {
		t.Fatalf("size with <half-slice free = %v, want 0", got)
	}
}
