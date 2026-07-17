package ridp

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// REVERTER — intraday mean reversion, the third RIDP pattern. Validated on 12 months of
// 1-minute data (see scratchpad test_meanrev*.py): on high-amplitude names, buying a dip
// >=1.5 sigma below a 15-minute rolling mean and exiting back at the mean is robustly positive
// out-of-sample once execution is commission-free with maker limit fills (~+15-27 bps/day on a
// $1,500 slice at 0-1 bps cost). The open question the backtest CAN'T answer is real fill
// quality on the wide-spread volatile names — so REVERTER runs LIVE-PAPER (real paper
// orders through the shared allocator, exchange stop at the z=-4 floor) to measure real
// fills before any real dollar is risked. This is the "Aggressive" config the operator
// picked (15-min mean, -1.5 sigma entry, exit at mean, always-on). Candidate pool: top
// reverterTopN by ATR% (fixed count — see ridp.go throughput dials).

const (
	reverterWindow   = 15   // rolling minutes for the mean/std band
	reverterZIn      = -1.5 // buy when price is <= this many sigma below the rolling mean
	reverterZOut     = 0.0  // exit when it reverts to >= the mean
	reverterZStop    = -4.0 // hard stop: the range broke, bail
	reverterSlice    = 1500 // $ per position
	reverterStartMin = 15   // no entries before 09:45 ET (need a full window of bars)
	reverterLastMin  = 375  // no fresh entries after 15:45 ET (flat at 15:55 like RIDER)
	// Candidate pool is reverterTopN (ridp.go var, default 55) — a FIXED count, not a
	// fraction: on the 2026-07-16 534-name universe a top-1/3 rule would have tripled
	// REVERTER's pond right after the scale incident. Same-size pond, swingier fish.
)

// zscore returns how many population-standard-deviations the last close sits from the mean of
// the series, plus that std (in price units, for setting the protective floor). ok=false when
// the window is flat/degenerate. Pure so it is unit-testable.
func zscore(closes []float64) (z, std float64, ok bool) {
	n := len(closes)
	if n < 2 {
		return 0, 0, false
	}
	var sum, sq float64
	for _, c := range closes {
		sum += c
		sq += c * c
	}
	fn := float64(n)
	mean := sum / fn
	varc := sq/fn - mean*mean
	if varc <= 0 {
		return 0, 0, false
	}
	std = math.Sqrt(varc)
	if std <= 0 {
		return 0, 0, false
	}
	return (closes[n-1] - mean) / std, std, true
}

// reverterZ computes the live z-score (and band std) for a symbol from the last reverterWindow
// 1-minute bars.
func (m *Manager) reverterZ(sym string) (z, last, std float64, ok bool) {
	bars := m.engine.Snapshot(sym, 1)
	if len(bars) < reverterWindow {
		return 0, 0, 0, false
	}
	w := bars[len(bars)-reverterWindow:]
	cl := make([]float64, len(w))
	for i, b := range w {
		cl[i] = b.Close
	}
	zz, sd, good := zscore(cl)
	if !good {
		return 0, 0, 0, false
	}
	return zz, cl[len(cl)-1], sd, true
}

// reverterEligible returns the high-amplitude subset (top reverterTopN of the universe by
// ATR/price) — the persistent selector: range SIZE, not "choppiness", predicts week to week.
func (m *Manager) reverterEligible() map[string]bool {
	m.mu.Lock()
	atrs := make(map[string]float64, len(m.daily))
	for sym, d := range m.daily {
		if d.ATR > 0 {
			atrs[sym] = d.ATR
		}
	}
	m.mu.Unlock()
	type ap struct {
		sym string
		amp float64
	}
	list := make([]ap, 0, len(atrs))
	for sym, atr := range atrs {
		if px := m.lastPrice(sym); px > 0 {
			list = append(list, ap{sym, atr / px})
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].amp > list[j].amp })
	keep := reverterTopN
	if keep > len(list) {
		keep = len(list)
	}
	if keep < 1 {
		keep = 1
	}
	out := make(map[string]bool, keep)
	for i := 0; i < keep; i++ {
		out[list[i].sym] = true
	}
	return out
}

// scanReverterEntries buys high-amplitude names dipping >=1.5 sigma below their 15-minute
// rolling mean — real paper orders through the shared allocator, with an exchange-side stop at
// the z=-4 floor. No per-strategy trade cap: only the account budget (allocator) limits it.
func (m *Manager) scanReverterEntries(now time.Time) {
	elig := m.reverterEligible()
	if len(elig) == 0 {
		return
	}
	for _, sym := range m.symbols {
		if sym == "QQQ" || sym == "SPY" || sym == "SMH" || !elig[sym] {
			continue
		}
		m.mu.Lock()
		_, held := m.open[sym] // one position per symbol across strategies
		m.mu.Unlock()
		if held {
			continue
		}
		z, last, std, ok := m.reverterZ(sym)
		if !ok || last <= 0 || std <= 0 || z > reverterZIn {
			continue
		}
		qty := float64(int(reverterSlice / last))
		if qty < 1 {
			continue
		}
		// z=-4 disaster floor: the mean sits ~+1.5σ above entry, so z=-4 ≈ entry − 2.5σ.
		stopPrice := round2(last - 2.5*std)
		if stopPrice <= 0 {
			continue
		}
		okAlloc, why := m.alloc("reverter", qty*last)
		if !okAlloc {
			m.journalSkip("reverter", sym, why)
			continue
		}
		m.journal("reverter", "signal", sym,
			fmt.Sprintf("z=%.2f dip below %dm mean, buy %.0f @ ~$%.2f, floor $%.2f", z, reverterWindow, qty, last, stopPrice))
		m.openPosition("reverter", sym, qty, std, stopPrice, now)
	}
}

// manageReverter exits open reverter positions: back at the mean (z>=0), hard stop (z<=-4), or
// the 15:55 flatten. Exits market-sell through the shared closePosition (cancels the exchange
// stop first). The exchange stop is the disaster floor if this loop is ever down.
func (m *Manager) manageReverter(now time.Time) {
	m.mu.Lock()
	positions := make([]*Position, 0)
	for _, p := range m.open {
		if p.Strategy == "reverter" {
			positions = append(positions, p)
		}
	}
	m.mu.Unlock()
	if len(positions) == 0 {
		return
	}
	flat := now.Hour() > riderFlatHour || (now.Hour() == riderFlatHour && now.Minute() >= riderFlatMin)
	for _, p := range positions {
		if closed, px := m.exchangeClosed(p); closed {
			m.finalize(p, px, "exchange stop filled (range broke)")
			continue
		}
		m.ensureProtection(p)
		if flat {
			m.closePosition(p, "15:55 flatten")
			continue
		}
		z, last, _, ok := m.reverterZ(p.Symbol)
		if !ok || last <= 0 {
			continue
		}
		switch {
		case z >= reverterZOut:
			m.closePosition(p, "reverted to mean")
		case z <= reverterZStop:
			m.closePosition(p, fmt.Sprintf("range broke (z=%.1f)", z))
		}
	}
}
