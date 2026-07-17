package ridp

import (
	"fmt"
	"time"
)

// scanDipperEntries fires in the 09:31-09:50 ET window: any symbol whose DAILY bars show
// a qualified falling setup (3+ red closes or -6% in 5 sessions) followed by yesterday
// CLOSING above the prior day's high (the turn) is bought at this morning's market.
// Risk-based sizing: qty = $50 / (2 x ATR14), notional-capped; hard GTC stop at
// entry - 2 x ATR rides on the exchange for the whole multi-week hold.
func (m *Manager) scanDipperEntries(now time.Time) {
	today := now.Format("2006-01-02")
	for _, sym := range m.symbols {
		if sym == "QQQ" || sym == "SPY" || sym == "SMH" {
			continue
		}
		m.mu.Lock()
		d := m.daily[sym]
		_, held := m.open[sym]
		fired := m.entered["dipper|"+sym+"|"+today]
		ghostQty := m.livePos[sym] // untracked shares present = don't pyramid onto a leak
		cooling := time.Since(m.lastExit[sym]) < 90*time.Second
		m.mu.Unlock()
		if held || fired || cooling || ghostQty >= 1 || d == nil || !d.Triggered || d.ATR <= 0 || d.AsOf != today {
			continue
		}
		last := m.lastPrice(sym)
		if last <= 0 {
			continue
		}
		risk := dipperStopATR * d.ATR
		qty := float64(int(dipperRiskUSD / risk))
		if qty < 1 {
			m.journal("dipper", "skip", sym, fmt.Sprintf("ATR $%.2f too large for $%.0f risk (needs fractional)", d.ATR, dipperRiskUSD))
			m.markDipperFired(sym, today)
			continue
		}
		if qty*last > dipperMaxNotnl {
			qty = float64(int(dipperMaxNotnl / last))
		}
		if qty < 1 {
			m.journal("dipper", "skip", sym, fmt.Sprintf("price $%.2f > notional cap $%.0f", last, dipperMaxNotnl))
			m.markDipperFired(sym, today)
			continue
		}
		ok, why := m.alloc("dipper", qty*last)
		if !ok {
			m.journal("dipper", "skip", sym, why)
			m.markDipperFired(sym, today) // slots are day-scarce for swings; don't spin
			continue
		}
		m.markDipperFired(sym, today)
		hardStop := round2(last - risk)
		m.journal("dipper", "signal", sym,
			fmt.Sprintf("turn confirmed (closed above prior high after the fall); ATR $%.2f, stop $%.2f", d.ATR, hardStop))
		m.openPosition("dipper", sym, qty, d.ATR, hardStop, now)
	}
}

func (m *Manager) markDipperFired(sym, day string) {
	m.mu.Lock()
	m.entered["dipper|"+sym+"|"+day] = true
	m.mu.Unlock()
}

// manageDipper runs the swing exits: the hard stop lives on the exchange (GTC), so this
// loop only (a) detects that the stop filled, (b) advances the highest-close peak once
// per session, (c) exits when price falls 2.5 x ATR below that peak, and (d) enforces the
// 40-session max hold. DIPPER positions hold overnight by design — no EOD flatten.
func (m *Manager) manageDipper(now time.Time, sessionMin int) {
	m.mu.Lock()
	positions := make([]*Position, 0)
	for _, p := range m.open {
		if p.Strategy == "dipper" {
			positions = append(positions, p)
		}
	}
	m.mu.Unlock()
	if len(positions) == 0 {
		return
	}
	today := now.Format("2006-01-02")
	lateDay := sessionMin >= 375 // 15:45+: today's price is close enough to a "close"
	for _, p := range positions {
		if m.resolveClosing(p) {
			continue // exit in flight — nothing else may touch this position
		}
		if closed, px := m.exchangeClosed(p); closed {
			m.finalize(p, px, "hard stop filled (exchange)")
			continue
		}
		m.ensureProtection(p)
		last := m.lastPrice(p.Symbol)
		if last <= 0 {
			continue
		}
		changed := false
		// session accounting + peak-close ratchet, once per session (near the close so
		// the "highest close" convention from the 5-year test is preserved)
		if lateDay && p.LastDay != today {
			p.LastDay = today
			p.Sessions++
			changed = true
			if last > p.Peak {
				p.Peak = last
			}
		}
		if last > p.Peak && lateDay {
			p.Peak = last
			changed = true
		}
		trail := p.Peak - dipperTrailATR*p.ATR
		if last <= trail && p.Peak > p.Entry {
			m.closePosition(p, fmt.Sprintf("trail 2.5xATR hit (peak $%.2f)", p.Peak))
			continue
		}
		if p.Sessions >= dipperMaxHold {
			m.closePosition(p, "max hold 40 sessions")
			continue
		}
		if changed {
			m.saveState()
		}
	}
}
