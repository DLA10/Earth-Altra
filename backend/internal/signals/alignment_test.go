package signals

import "testing"

// TestAlignmentThroughput pins the 2026-07-16 throughput-mode default: only the
// proven-negative evidence blocks — the −228R toxic cell (mkt-up/sym-down, except
// orb_breakout) and fh_reversal (negative everywhere). All other cells trade.
func TestAlignmentThroughput(t *testing.T) {
	cases := []struct {
		strat        string
		mktUp, symUp bool
		want         bool
	}{
		{"vwap_reclaim", false, false, true},
		{"vwap_reclaim", true, true, true},
		{"vwap_reclaim", true, false, false}, // the toxic cell
		{"vwap_reclaim", false, true, true},  // opened by throughput mode
		{"momentum_cont", true, true, true},
		{"momentum_cont", false, false, true}, // opened by throughput mode
		{"momentum_cont", true, false, false}, // the toxic cell
		{"dip_bounce", false, false, true},
		{"dip_bounce", true, false, false}, // the toxic cell
		{"dip_bounce", false, true, true},  // opened by throughput mode
		{"rel_strength", false, true, true},
		{"rel_strength", true, true, true},   // opened by throughput mode
		{"rel_strength", true, false, false}, // the toxic cell
		{"orb_breakout", true, false, true},  // unrestricted, even in the toxic cell
		{"fh_reversal", false, false, false}, // retired everywhere
		{"some_future_strat", true, true, true},  // unknown strategies fail open
		{"some_future_strat", true, false, false}, // ... except in the toxic cell
	}
	for _, c := range cases {
		if got := AlignmentAllowed(c.strat, c.mktUp, c.symUp); got != c.want {
			t.Errorf("AlignmentAllowed(%s, mktUp=%v, symUp=%v) = %v, want %v",
				c.strat, c.mktUp, c.symUp, got, c.want)
		}
	}
	if trendCell(false, false) != "mkt-down/sym-down" || trendCell(true, true) != "mkt-up/sym-up" {
		t.Error("trendCell rendering wrong")
	}
}

// TestAlignmentStrictTable pins the original validated best-cell-only playbook, which
// QUANT_ALIGN_STRICT=true restores verbatim (the rollback path).
func TestAlignmentStrictTable(t *testing.T) {
	cases := []struct {
		strat        string
		mktUp, symUp bool
		want         bool
	}{
		{"vwap_reclaim", false, false, true},
		{"vwap_reclaim", true, true, true},
		{"vwap_reclaim", true, false, false},
		{"vwap_reclaim", false, true, false},
		{"momentum_cont", true, true, true},
		{"momentum_cont", false, false, false},
		{"dip_bounce", false, false, true},
		{"dip_bounce", true, false, false}, // the toxic cell
		{"rel_strength", false, true, true},
		{"rel_strength", true, true, false},
		{"orb_breakout", true, false, true},      // unrestricted
		{"fh_reversal", false, false, false},     // retired everywhere
		{"some_future_strat", true, false, true}, // unknown strategies fail open
	}
	for _, c := range cases {
		if got := alignmentStrict(c.strat, c.mktUp, c.symUp); got != c.want {
			t.Errorf("alignmentStrict(%s, mktUp=%v, symUp=%v) = %v, want %v",
				c.strat, c.mktUp, c.symUp, got, c.want)
		}
	}
}
