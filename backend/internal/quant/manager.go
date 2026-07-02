package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// managedPos is one live position the manager is actively running Agent 3 on.
type managedPos struct {
	symbol      string
	qty         float64
	entryPrice  float64
	entryTime   time.Time
	stopOrderID string
	stopPrice   float64 // current FIXED stop level (0 while on the initial trailing stop)
	conf        float64
}

// Manager owns the live position lifecycle: it places the entry, the deterministic trailing-stop
// floor (so the position is protected sub-second on Alpaca regardless of Agent 3), then runs the
// Agent 3 exit loop, executes its verbs as real orders, and releases capital when the position
// closes. The floor guarantees no position is ever left unmanaged.
type Manager struct {
	eng          *Engine
	broker       *Broker
	agent3       *Agent3
	trailPct     float64
	overnightCap float64 // max position VALUE allowed past the close (0 = flatten all)

	// OnClosed, when set, receives an APPROXIMATE realized P&L for every closed position
	// (marked to the last engine price at close detection). Feeds the daily loss-cap
	// tracker; the authoritative P&L remains the broker reconstruction.
	OnClosed func(sym string, approxPNL float64)

	mu        sync.Mutex
	open      map[string]*managedPos
	keeperDay string // ET day the overnight keeper was chosen for
	keeperSym string // the single position allowed to hold overnight ("" = none)
}

func NewManager(eng *Engine, broker *Broker, agent3 *Agent3, trailPct, overnightCap float64) *Manager {
	if trailPct <= 0 {
		trailPct = 1.5
	}
	return &Manager{eng: eng, broker: broker, agent3: agent3, trailPct: trailPct,
		overnightCap: overnightCap, open: map[string]*managedPos{}}
}

// OpenSymbols lists symbols currently being managed.
func (m *Manager) OpenSymbols() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.open))
	for s := range m.open {
		out = append(out, s)
	}
	return out
}

// Open executes an approved dip buy (the dipwatch pipeline's entry point).
func (m *Manager) Open(ctx context.Context, de DipEvent, conf, dollars float64) {
	m.OpenPosition(ctx, de.Symbol, conf, dollars)
}

// OpenPosition executes an approved buy for any pipeline (dip or signal engine): market
// entry → confirm fill → place the trailing-stop floor → register → run the Agent 3 loop.
// Releases the reserved capital on any pre-registration failure.
func (m *Manager) OpenPosition(ctx context.Context, sym string, conf, dollars float64) {
	registered := false
	defer func() {
		if !registered {
			m.eng.alloc.Release(sym) // never opened → give the capital back
		}
	}()

	price := m.eng.LastClose(sym)
	if price <= 0 {
		m.log("agent3_exit", "error", sym, "no price for entry")
		return
	}
	qty := math.Floor(dollars / price)
	if qty < 1 {
		m.log("agent3_exit", "skip", sym, "size < 1 whole share")
		return
	}
	entryCoid := fmt.Sprintf("%s__%s__entry__%d", QuantStrategy, sym, time.Now().UnixNano())
	id, err := m.broker.MarketBuy(sym, qty, entryCoid)
	if err != nil {
		m.log("agent3_exit", "error", sym, "entry failed: "+err.Error())
		return
	}
	_, ap := m.awaitFill(id, 12*time.Second)
	// Size the stop to what we ACTUALLY hold (a market order may finish filling after the first
	// poll; sizing to a partial fill would leave the rest unprotected).
	fq, _ := m.broker.PositionQty(sym)
	if fq <= 0 {
		m.log("agent3_exit", "error", sym, "entry not confirmed filled (no position)")
		return
	}
	if ap <= 0 {
		ap = price
	}

	// Deterministic protective floor: a trailing stop that follows price up. If it can't be
	// placed, immediately exit so we never hold an unprotected position.
	stopCoid := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, sym, time.Now().UnixNano())
	stopID, serr := m.broker.TrailingStopSell(sym, fq, m.trailPct, stopCoid)
	if serr != nil {
		log.Printf("[quant] %s trailing-stop failed (%v) — exiting to stay protected", sym, serr)
		exitCoid := fmt.Sprintf("%s__%s__exit__No_Stop__%d", QuantStrategy, sym, time.Now().UnixNano())
		if _, e := m.broker.MarketSell(sym, fq, exitCoid); e != nil {
			log.Printf("[quant] CRITICAL: %s held with NO stop and exit sell failed: %v", sym, e)
		}
		return
	}

	// Initialize the tracked stop level to the trailing floor's starting point so Agent 3's first
	// tighten can't accidentally LOOSEN protection (and the exit snapshot shows a real stop).
	pos := &managedPos{symbol: sym, qty: fq, entryPrice: ap, entryTime: time.Now(), stopOrderID: stopID,
		stopPrice: round2(ap * (1 - m.trailPct/100)), conf: conf}
	m.mu.Lock()
	m.open[sym] = pos
	m.mu.Unlock()
	registered = true
	m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "order", Symbol: sym,
		Note: fmt.Sprintf("entry %.0f @ $%.2f; trailing stop %.1f%% placed", fq, ap, m.trailPct)})
	log.Printf("[quant] ENTER %s %.0f @ $%.2f (conf %.2f); trailing stop %.1f%%", sym, fq, ap, conf, m.trailPct)
	m.manage(ctx, pos)
}

func (m *Manager) awaitFill(id string, max time.Duration) (float64, float64) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		fq, ap, status, err := m.broker.Order(id)
		if err == nil && fq > 0 && ap > 0 && (status == "filled" || status == "partially_filled") {
			return fq, ap
		}
		time.Sleep(800 * time.Millisecond)
	}
	return 0, 0
}

// manage runs the per-position exit loop until the position closes or ctx ends.
func (m *Manager) manage(ctx context.Context, pos *managedPos) {
	sym := pos.symbol
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.cadence(pos)):
		}

		// Closed on the exchange (the stop filled, or a prior exit completed)?
		if qty, err := m.broker.PositionQty(sym); err == nil && qty <= 0 {
			m.close(pos, "closed on exchange (stop/exit filled)")
			return
		}

		// End-of-day flatten (intraday only), from 15:55 ET onward — NOT just until 16:00,
		// or a flatten that kept deferring through those five minutes would silently give up
		// at the close and hold overnight. Only release the slot once we're confirmed flat;
		// otherwise retry on the next pass (the position stays protected meanwhile).
		now := time.Now().In(m.eng.loc)
		if now.Hour() > 15 || (now.Hour() == 15 && now.Minute() >= 55) {
			// Capped overnight hold: at most ONE position — the best-performing winner
			// whose value fits under the cap — may ride through the close (its GTC
			// trailing stop stays live on the exchange). Everything else flattens.
			if m.mayHoldOvernight(sym) {
				continue
			}
			if now.Hour() >= 16 {
				log.Printf("[quant] %s still not flat after the close — retrying EOD exit", sym)
			}
			if m.forceExit(pos, "EOD_Force") {
				m.close(pos, "EOD flatten")
				return
			}
			continue
		}

		// No Agent 3? The trailing stop manages it on its own.
		if m.agent3 == nil || !m.agent3.Enabled() {
			continue
		}

		snap := m.eng.exitSnapshot(sym, pos.entryPrice, pos.qty, pos.stopPrice, pos.entryTime)
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		dec, usage, err := m.agent3.Decide(dctx, snap)
		cancel()
		if err != nil {
			m.log("agent3_exit", "error", sym, err.Error())
			continue
		}
		m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "decision", Symbol: sym, Model: m.agent3.model,
			Input: json.RawMessage(snap), Output: dec, Tokens: &usage})
		if m.execute(pos, dec) {
			return // position closed by a take-profit / exit_now
		}
	}
}

// cadence is adaptive: faster when price is near the stop (the moment that matters), slower when calm.
func (m *Manager) cadence(pos *managedPos) time.Duration {
	cur := m.eng.LastClose(pos.symbol)
	if pos.stopPrice > 0 && cur > 0 {
		if (cur-pos.stopPrice)/cur < 0.005 { // within 0.5% of the stop
			return 5 * time.Second
		}
	}
	return 12 * time.Second
}

// execute turns Agent 3's verb into real orders (always keeping a protective stop in place).
// Returns true if the position was closed (so the manage loop can stop).
func (m *Manager) execute(pos *managedPos, d ExitDecision) (closed bool) {
	sym := pos.symbol
	cur := m.eng.LastClose(sym)
	switch d.Action {
	case ExitHold:
		return false
	case ExitTightenStop:
		// Ratchet UP only, and the new stop must sit below the market.
		if d.StopPrice <= 0 || cur <= 0 || d.StopPrice <= pos.stopPrice || d.StopPrice >= cur {
			return false
		}
		// Confirm the old stop is gone before placing a new one (else two stops → oversell).
		if !m.cancelAndConfirm(pos.stopOrderID) {
			return false // old stop filled/unconfirmed — don't stack a second stop
		}
		coid := fmt.Sprintf("%s__%s__exit__Stop__%d", QuantStrategy, sym, time.Now().UnixNano())
		id, err := m.broker.StopSell(sym, pos.qty, d.StopPrice, coid)
		if err != nil {
			// Re-place a trailing stop so we're never left unprotected.
			tc := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, sym, time.Now().UnixNano())
			if pid, perr := m.broker.TrailingStopSell(sym, pos.qty, m.trailPct, tc); perr == nil {
				pos.stopOrderID = pid
				pos.stopPrice = 0
			}
			return false
		}
		pos.stopOrderID = id
		pos.stopPrice = d.StopPrice
		m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "order", Symbol: sym,
			Note: fmt.Sprintf("stop tightened to $%.2f — %s", d.StopPrice, d.Reason)})
		return false
	case ExitTakeProfit:
		if m.forceExit(pos, "Take_Profit") {
			m.close(pos, "take-profit exit")
			return true
		}
		return false
	case ExitNow:
		if m.forceExit(pos, "AI_Exit") {
			m.close(pos, "AI exit")
			return true
		}
		return false
	}
	return false
}

// cancelAndConfirm cancels an order and waits until it is no longer live (Alpaca's cancel is
// ASYNC). Returns true only when the order is confirmed gone (canceled/expired/etc.). Returns
// false if it already FILLED or the state can't be confirmed — in both cases the caller must NOT
// also market-sell, or it would oversell into a short.
func (m *Manager) cancelAndConfirm(orderID string) bool {
	if orderID == "" {
		return true
	}
	_ = m.broker.Cancel(orderID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, status, err := m.broker.Order(orderID); err == nil {
			switch status {
			case "canceled", "expired", "rejected", "done_for_day", "replaced":
				return true
			case "filled":
				return false // already executed — do not sell again
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false // unconfirmed → assume still live; don't risk a double sell
}

// forceExit flattens the position now: confirm-cancel the protective stop, then market-sell the
// ACTUAL held qty. If the stop already took the position (flat), there's nothing to sell. If the
// sell fails, it re-places a protective stop so the position is never left unprotected. Returns
// true only when the position is confirmed flat.
func (m *Manager) forceExit(pos *managedPos, reason string) bool {
	canceled := m.cancelAndConfirm(pos.stopOrderID)
	// Source of truth: what do we actually still hold?
	qty, err := m.broker.PositionQty(pos.symbol)
	if err == nil && qty <= 0 {
		return true // already flat (the stop filled)
	}
	if !canceled {
		// The stop is still live (or unconfirmable) and we still hold — selling now could
		// double up into a short. Leave the stop to do its job; retry on the next pass.
		log.Printf("[quant] %s force exit (%s) deferred: stop not confirmed canceled", pos.symbol, reason)
		return false
	}
	if qty <= 0 {
		qty = pos.qty // couldn't read position; fall back to known qty
	}
	coid := fmt.Sprintf("%s__%s__exit__%s__%d", QuantStrategy, pos.symbol, reason, time.Now().UnixNano())
	if _, serr := m.broker.MarketSell(pos.symbol, qty, coid); serr != nil {
		log.Printf("[quant] %s force exit (%s) sell failed: %v — re-placing protective stop", pos.symbol, reason, serr)
		tc := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, pos.symbol, time.Now().UnixNano())
		if pid, perr := m.broker.TrailingStopSell(pos.symbol, qty, m.trailPct, tc); perr == nil {
			pos.stopOrderID = pid
			pos.stopPrice = 0
		}
		return false
	}
	m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "order", Symbol: pos.symbol, Note: "market exit (" + reason + ")"})
	log.Printf("[quant] EXIT %s reason=%s qty=%.0f", pos.symbol, reason, qty)
	return true
}

// mayHoldOvernight reports whether sym is today's designated overnight keeper: the single
// open position with the best unrealized P&L that (a) is actually PROFITABLE and (b) fits
// under the overnight value cap. Chosen once per ET day at the first EOD pass so the
// per-position manage goroutines agree; losers are never held overnight.
func (m *Manager) mayHoldOvernight(sym string) bool {
	if m.overnightCap <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	day := time.Now().In(m.eng.loc).Format("2006-01-02")
	if m.keeperDay != day {
		m.keeperDay = day
		m.keeperSym = ""
		best := 0.0
		for s, p := range m.open {
			cur := m.eng.LastClose(s)
			if cur <= 0 {
				continue
			}
			if val := cur * p.qty; val > m.overnightCap {
				continue
			}
			if pnl := (cur - p.entryPrice) * p.qty; pnl > best {
				best = pnl
				m.keeperSym = s
			}
		}
		if m.keeperSym != "" {
			log.Printf("[quant] overnight keeper: %s (≤ $%.0f cap, in profit); all other positions flatten", m.keeperSym, m.overnightCap)
		}
	}
	return m.keeperSym == sym
}

func (m *Manager) close(pos *managedPos, note string) {
	sym := pos.symbol
	m.mu.Lock()
	delete(m.open, sym)
	m.mu.Unlock()
	m.eng.alloc.Release(sym)
	m.eng.logRec(LogRecord{Agent: "pipeline", Event: "outcome", Symbol: sym, Note: note})
	log.Printf("[quant] CLOSED %s — %s; capital released", sym, note)
	if m.OnClosed != nil {
		pnl := 0.0
		if px := m.eng.LastClose(sym); px > 0 {
			pnl = (px - pos.entryPrice) * pos.qty
		}
		m.OnClosed(sym, pnl)
	}
}

func (m *Manager) log(agent, event, sym, note string) {
	m.eng.logRec(LogRecord{Agent: agent, Event: event, Symbol: sym, Note: note})
}
