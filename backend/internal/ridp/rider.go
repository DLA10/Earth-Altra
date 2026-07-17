package ridp

import (
	"fmt"
	"sort"
	"time"
)

// scanRiderEntries looks for the day's leaders: above a rising VWAP on outsized volume,
// QQQ not falling. Since 2026-07-17 (operator directives): entries from 09:45 ET with an
// EARLY-STRICT ramp (the original +1%/2x gates until 10:00, throughput gates after);
// candidates are RANKED by momentum quality (gain x rvol) and funded strongest-first;
// seats are budget-limited only (no slot cap on paper money); a shaken-out symbol may
// RE-BOARD up to riderMaxEntries/day, but only above its previous run's peak — a new
// high proves the drop was minute noise, not a reversal.
func (m *Manager) scanRiderEntries(now time.Time, sessionMin int) {
	// Throughput mode: "QQQ not falling" (>= riderQQQMin from open, default -0.15%)
	// instead of the original strictly-green gate, which disabled RIDER ~half of all days.
	qqqOK := false
	if qo, ql := m.sessionOpenLast("QQQ"); qo > 0 && ql >= qo*(1+riderQQQMin) {
		qqqOK = true
	}
	if !qqqOK {
		return
	}
	// Early-strict ramp: in the first 15 minutes of the window (09:45-10:00 ET) demand
	// the ORIGINAL validated gates — early birds must be loud (real sector waves are
	// never marginal, and the VWAP is thin that early). Throughput gates after 10:00.
	gainMin, rvolMin := riderGainMin, riderRVOLMin
	if sessionMin < 30 {
		if gainMin < 0.01 {
			gainMin = 0.01
		}
		if rvolMin < 2.0 {
			rvolMin = 2.0
		}
	}
	type cand struct {
		sym              string
		last, gain, rvol float64
		qty              float64
	}
	sessionStart := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, m.etz).Unix()
	var cands []cand
	for _, sym := range m.symbols {
		if sym == "QQQ" || sym == "SPY" || sym == "SMH" {
			continue
		}
		m.mu.Lock()
		_, held := m.open[sym]
		cnt := m.riderCount[sym]
		exitPeak := m.riderExitPeak[sym]
		d := m.daily[sym]
		ghostQty := m.livePos[sym] // untracked shares present = don't pyramid onto a leak
		cooling := time.Since(m.lastExit[sym]) < 90*time.Second
		m.mu.Unlock()
		if held || cooling || ghostQty >= 1 || d == nil || d.AvgVol <= 0 || cnt >= riderMaxEntries {
			continue
		}
		bars := m.engine.Snapshot(sym, 1)
		if len(bars) < 35 {
			continue
		}
		var open, cumVol, pv, pvOld, volOld float64
		first := true
		cut := now.Unix() - 30*60
		for _, b := range bars {
			if b.Time < sessionStart || b.Volume <= 0 {
				continue
			}
			if first {
				open = b.Open
				first = false
			}
			tp := (b.High + b.Low + b.Close) / 3
			pv += tp * b.Volume
			cumVol += b.Volume
			if b.Time <= cut {
				pvOld += tp * b.Volume
				volOld += b.Volume
			}
		}
		if open <= 0 || cumVol <= 0 || volOld <= 0 {
			continue
		}
		last := bars[len(bars)-1].Close
		// Noise re-entry bar: after a shakeout, only re-board ABOVE the previous run's
		// peak. Below it, the "recovery" hasn't proven anything yet.
		if cnt >= 1 && (exitPeak <= 0 || last <= exitPeak) {
			continue
		}
		vwap := pv / cumVol
		vwapOld := pvOld / volOld
		if last/open-1 < gainMin || last <= vwap || vwap <= vwapOld {
			continue
		}
		// "2x normal for THIS time of day": compare cumulative volume to what a normal day
		// has traded by this minute using the symbol's learned intraday curve (U-shaped —
		// heavy at the open/close, light midday). Falls back to a flat estimate until the
		// profile lands. The old flat sessionMin/390 over-counted late-morning volume.
		frac := m.expectedVolFrac(sym, sessionMin)
		rvol := cumVol / (d.AvgVol * frac)
		if rvol < rvolMin {
			continue
		}
		qty := float64(int(riderSlice / last))
		if qty < 1 {
			m.journal("rider", "skip", sym, fmt.Sprintf("price $%.2f > slice $%d", last, riderSlice))
			m.markEntered(sym)
			continue
		}
		cands = append(cands, cand{sym: sym, last: last, gain: last/open - 1, rvol: rvol, qty: qty})
	}
	if len(cands) == 0 {
		return
	}
	// RANK: strongest momentum first (gain x rvol), so budget contention favors the
	// leaders instead of whichever symbol was scanned first. (2026-07-17: TFC took a
	// seat by scan order while MU/SNDK/ARM at +4-5% were skipped "no free rider slot";
	// TFC ended the book's only loser.)
	sort.Slice(cands, func(i, j int) bool { return cands[i].gain*cands[i].rvol > cands[j].gain*cands[j].rvol })
	for _, c := range cands {
		ok, why := m.alloc("rider", c.qty*c.last)
		if !ok {
			m.journalSkip("rider", c.sym, why)
			continue // do NOT mark entered — budget may free up later today
		}
		m.markEntered(c.sym)
		m.mu.Lock()
		nth := m.riderCount[c.sym]
		m.mu.Unlock()
		tag := ""
		if nth > 1 {
			tag = fmt.Sprintf(" — RE-BOARD #%d above prior peak", nth)
		}
		m.journal("rider", "signal", c.sym,
			fmt.Sprintf("+%.1f%% from open, rvol %.1f, above rising VWAP, QQQ ok (rank %.2f)%s",
				c.gain*100, c.rvol, c.gain*c.rvol*100, tag))
		m.openPosition("rider", c.sym, c.qty, 0, 0, now)
	}
}

func (m *Manager) markEntered(sym string) {
	m.mu.Lock()
	m.entered[sym] = true
	m.riderCount[sym]++
	m.mu.Unlock()
}

// manageRider runs the exit logic for open RIDER positions: software trail 3.5% from the
// intrabar peak, tightened to 2% once the peak is +3% above entry (the operator's
// protect-the-gain stage), and the 15:55 flatten. The exchange-side 3.5% trailing stop
// placed at entry remains the disaster floor if this process dies.
func (m *Manager) manageRider(now time.Time, sessionMin int) {
	m.mu.Lock()
	positions := make([]*Position, 0, len(m.open))
	for _, p := range m.open {
		if p.Strategy == "rider" {
			positions = append(positions, p)
		}
	}
	m.mu.Unlock()
	if len(positions) == 0 {
		return
	}
	flat := now.Hour() > riderFlatHour || (now.Hour() == riderFlatHour && now.Minute() >= riderFlatMin)
	for _, p := range positions {
		if m.resolveClosing(p) {
			continue // exit in flight — nothing else may touch this position
		}
		// closed on the exchange (trailing stop filled)?
		if closed, px := m.exchangeClosed(p); closed {
			m.finalize(p, px, "exchange trailing stop filled")
			continue
		}
		m.ensureProtection(p)
		if flat {
			m.closePosition(p, "15:55 flatten")
			continue
		}
		bars := m.engine.Snapshot(p.Symbol, 1)
		if len(bars) == 0 {
			continue
		}
		changed := false
		for _, b := range bars {
			if b.Time >= p.OpenedAt.Unix() && b.High > p.Peak {
				p.Peak = b.High
				changed = true
			}
		}
		if !p.Tightened && p.Peak >= p.Entry*(1+riderTightTrig) {
			p.Tightened = true
			changed = true
			m.journal("rider", "tighten", p.Symbol,
				fmt.Sprintf("peak +%.1f%% — trail tightened to %.1f%%", (p.Peak/p.Entry-1)*100, riderTightPct*100))
		}
		k := riderTrailPct
		if p.Tightened {
			k = riderTightPct
		}
		last := bars[len(bars)-1].Close
		if last > 0 && last <= p.Peak*(1-k) {
			m.closePosition(p, fmt.Sprintf("trail %.1f%% hit", k*100))
			continue
		}
		if changed {
			m.saveState()
		}
	}
}

// sessionOpenLast returns today's session open and latest price for a symbol.
func (m *Manager) sessionOpenLast(sym string) (open, last float64) {
	now := time.Now().In(m.etz)
	start := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, m.etz).Unix()
	for _, b := range m.engine.Snapshot(sym, 1) {
		if b.Time < start {
			continue
		}
		if open == 0 {
			open = b.Open
		}
		last = b.Close
	}
	return
}
