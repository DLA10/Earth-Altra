// SURGER desk manager: three detector variants trading LIVE paper on the dip+rise
// account (PAPER_DIP keys). The account is SHARED, so the invariants here are stricter
// than the dedicated-account desks:
//
//  1. ATTRIBUTION — every order carries an srg<variant>_ client-order-id prefix. The
//     quant desk's Reconstruct filters on its own QuantDip__ prefix and its Rehydrate
//     skips foreign-desk prefixes, so the books can never bleed into each other.
//  2. EXCLUSIVITY — a symbol is entered only if (a) no SURGER variant holds or is
//     entering it, and (b) the ACCOUNT holds zero shares of it (so we can never touch
//     a dip+rise position, and an opposite-side resting order can never wash-trade us).
//  3. NO GHOSTS — every order is journaled BEFORE placement and settled to a TERMINAL
//     state after; entries book exactly the filled quantity; exits sell exactly OUR
//     quantity (never account-wide on a shared account); unfinished orders are settled
//     at boot from the persisted in-flight list. A position is never dropped from the
//     book until its shares are confirmed gone.
//  4. NEVER NAKED — exchange stop from entry, confirmed-cancel ratchets (trail 3.5% →
//     2.0% after +3.5% peak), underwater-stop → flatten, EOD flat 15:55+.
package surger

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"live-optimus/backend/internal/quant"
)

const (
	trailWide    = 0.035
	trailTight   = 0.020
	tightenAt    = 0.035
	ratchetMinUp = 1.0015 // move the stop only for a >=0.15% improvement (order-churn bound)
	entryFromMin = 10 * 60
	entryToMin   = 15*60 + 30
	eodFlatMin   = 15*60 + 55
	cooldownMin  = 30
)

// Position is one open variant position with full exit + attribution state.
type Position struct {
	Variant    int       `json:"variant"`
	Symbol     string    `json:"symbol"`
	Qty        float64   `json:"qty"`
	EntryPrice float64   `json:"entry_price"`
	OpenedAt   time.Time `json:"opened_at"`
	Peak       float64   `json:"peak"`
	StopID     string    `json:"stop_id"`
	StopPx     float64   `json:"stop_px"`
	SignalPx   float64   `json:"signal_px"`       // detector bar close (slippage baseline)
	EntrySlip  float64   `json:"entry_slip_bps"`  // fill vs signal close
	HighPx     float64   `json:"high_px"`
	LowPx      float64   `json:"low_px"`
}

// Trade is a closed round-trip, pre-labeled for analysis.
type Trade struct {
	Variant    int       `json:"variant"`
	Symbol     string    `json:"symbol"`
	Qty        float64   `json:"qty"`
	EntryPrice float64   `json:"entry_price"`
	ExitPrice  float64   `json:"exit_price"`
	PnL        float64   `json:"pnl"`
	Reason     string    `json:"reason"`
	OpenedAt   time.Time `json:"opened_at"`
	ClosedAt   time.Time `json:"closed_at"`
	SignalPx   float64   `json:"signal_px,omitempty"`
	EntrySlip  float64   `json:"entry_slip_bps,omitempty"`
	HighPx     float64   `json:"high_px,omitempty"`
	LowPx      float64   `json:"low_px,omitempty"`
}

// inflight is an order journaled before placement so a crash can never orphan it.
type inflight struct {
	Variant int       `json:"variant"`
	Symbol  string    `json:"symbol"`
	Coid    string    `json:"coid"`
	OrderID string    `json:"order_id"`
	Kind    string    `json:"kind"` // entry | exit
	Qty     float64   `json:"qty"`
	At      time.Time `json:"at"`
}

type book struct {
	Open     map[string]*Position `json:"open"`
	Trades   []Trade              `json:"trades"`
	cooldown map[string]time.Time
}

// Manager runs the three variants. All book state behind mu.
type Manager struct {
	mu       sync.Mutex
	broker   *quant.Broker
	etz      *time.Location
	dataDir  string
	live     bool
	notional float64
	slots    int

	universe map[string]bool
	series   map[string]*series
	books    [NumVariants]*book
	pending  map[string]int      // symbol -> variant currently entering (exclusivity)
	inflight map[string]inflight // coid -> order awaiting settlement (persisted)
}

func New(broker *quant.Broker, etz *time.Location, dataDir string, live bool,
	universe []string, notional float64, slots int) *Manager {
	if etz == nil {
		etz = time.UTC
	}
	if notional <= 0 {
		notional = 5000
	}
	if slots <= 0 {
		slots = 5
	}
	m := &Manager{
		broker: broker, etz: etz, dataDir: filepath.Join(dataDir, "surger"),
		live: live, notional: notional, slots: slots,
		universe: map[string]bool{}, series: map[string]*series{},
		pending: map[string]int{}, inflight: map[string]inflight{},
	}
	for _, s := range universe {
		m.universe[s] = true
	}
	for i := range m.books {
		m.books[i] = &book{Open: map[string]*Position{}, cooldown: map[string]time.Time{}}
	}
	_ = os.MkdirAll(m.dataDir, 0755)
	m.loadState()
	return m
}

func (m *Manager) Enabled() bool { return m != nil && m.broker.Enabled() }

// Start settles anything left in-flight by a crash, re-verifies every open position's
// protection, then runs the 5s upkeep loop (stop-fill detection, EOD flatten).
func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		log.Printf("surger: disabled (dip+rise broker not enabled)")
		return
	}
	m.settleInflightBoot()
	m.rehydrate()
	log.Printf("surger: started LIVE(paper, dip+rise account) — 3 variants, $%.0f/slice, %d slots each, universe %d",
		m.notional, m.slots, len(m.universe))
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.tick()
			}
		}
	}()
}

// ---------------- bar feed (from the single SIP stream, completed bars only) ----------------

// OnBar consumes one completed 1-minute bar. Called from the stream goroutine — keep fast;
// order placement is dispatched to a goroutine per entry.
func (m *Manager) OnBar(sym string, ts time.Time, o, h, l, c, v float64) {
	if m == nil || !m.universe[sym] {
		return
	}
	day := ts.In(m.etz).Format("2006-01-02")
	minute := etMinute(ts, m.etz)

	m.mu.Lock()
	sr := m.series[sym]
	if sr == nil || sr.day != day {
		sr = newSeries(day)
		m.series[sym] = sr
	}
	sr.append(minute, o, h, l, c, v)
	// manage any open positions on this symbol at bar close
	for vi := range m.books {
		if p := m.books[vi].Open[sym]; p != nil {
			m.manageBarLocked(vi, p, c, h, l)
		}
	}
	var fire [NumVariants]bool
	tradable := minute >= entryFromMin && minute <= entryToMin
	if tradable {
		fire = sr.signals()
	}
	m.mu.Unlock()

	if !tradable {
		return
	}
	for vi := 0; vi < NumVariants; vi++ { // priority order C2 > C1 > SPECTRAL
		if fire[vi] {
			if m.tryReserve(vi, sym) {
				go m.enter(vi, sym, c)
			}
			break // exclusivity: first variant to fire owns the symbol
		}
	}
}

// tryReserve applies every entry precondition under one lock and reserves the symbol.
func (m *Manager) tryReserve(vi int, sym string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.books {
		if m.books[i].Open[sym] != nil {
			return false
		}
	}
	if _, pend := m.pending[sym]; pend {
		return false
	}
	b := m.books[vi]
	if t, ok := b.cooldown[sym]; ok && time.Since(t) < cooldownMin*time.Minute {
		return false
	}
	np := 0
	for s2, v2 := range m.pending {
		_ = s2
		if v2 == vi {
			np++
		}
	}
	if len(b.Open)+np >= m.slots {
		return false
	}
	m.pending[sym] = vi
	return true
}

func (m *Manager) release(sym string) {
	m.mu.Lock()
	delete(m.pending, sym)
	m.mu.Unlock()
}

// ---------------- entry ----------------

func (m *Manager) enter(vi int, sym string, signalPx float64) {
	defer m.release(sym)
	if !m.live {
		m.journal(vi, "shadow", sym, fmt.Sprintf("would BUY @ ~$%.2f", signalPx))
		return
	}
	// SHARED-ACCOUNT exclusivity: never touch a symbol anything else on this account holds.
	if aq, err := m.broker.PositionQty(sym); err != nil || aq != 0 {
		if err == nil {
			m.journal(vi, "skip", sym, fmt.Sprintf("account already holds %.0f (dip+rise or sibling) — exclusivity skip", aq))
		}
		return
	}
	qty := float64(int(m.notional / signalPx))
	if qty < 1 {
		return
	}
	if acc, err := m.broker.Account(); err == nil && acc.BuyingPower < qty*signalPx {
		m.journal(vi, "skip", sym, fmt.Sprintf("buying power $%.0f < $%.0f", acc.BuyingPower, qty*signalPx))
		return
	}

	coid := fmt.Sprintf("%s_%s_entry_%d", VariantCoid[vi], sym, time.Now().UnixNano())
	m.addInflight(inflight{Variant: vi, Symbol: sym, Coid: coid, Kind: "entry", Qty: qty, At: time.Now()})
	id, err := m.broker.MarketBuy(sym, qty, coid)
	if err != nil {
		m.dropInflight(coid)
		m.journal(vi, "error", sym, "entry rejected: "+err.Error())
		return
	}
	m.setInflightID(coid, id)

	fq, fp := m.settleOrder(id, 12*time.Second, true)
	m.dropInflight(coid)
	if fq < 1 {
		m.journal(vi, "skip", sym, "entry did not fill in time — canceled, nothing booked")
		return
	}
	price := fp
	if price <= 0 {
		price = signalPx
	}

	stopPx := round2(price * (1 - trailWide))
	sc := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[vi], sym, time.Now().UnixNano())
	stopID, sErr := m.broker.StopSell(sym, fq, stopPx, sc)
	if sErr != nil {
		// never naked: flatten what we just bought
		m.journal(vi, "error", sym, "protective stop failed ("+sErr.Error()+") — flattening for safety")
		fc := fmt.Sprintf("%s_%s_exit_%d", VariantCoid[vi], sym, time.Now().UnixNano())
		if xid, xerr := m.broker.MarketSell(sym, fq, fc); xerr == nil {
			xq, xp := m.settleOrder(xid, 12*time.Second, false)
			_ = xq
			m.mu.Lock()
			m.books[vi].Trades = append(m.books[vi].Trades, Trade{
				Variant: vi, Symbol: sym, Qty: fq, EntryPrice: price,
				ExitPrice: pick(xp, price), PnL: (pick(xp, price) - price) * fq,
				Reason: "safety_exit", OpenedAt: time.Now(), ClosedAt: time.Now(),
			})
			m.books[vi].cooldown[sym] = time.Now()
			m.saveStateLocked()
			m.mu.Unlock()
		}
		return
	}

	slip := (price/signalPx - 1) * 1e4
	m.mu.Lock()
	m.books[vi].Open[sym] = &Position{
		Variant: vi, Symbol: sym, Qty: fq, EntryPrice: price, OpenedAt: time.Now(),
		Peak: price, StopID: stopID, StopPx: stopPx,
		SignalPx: signalPx, EntrySlip: slip, HighPx: price, LowPx: price,
	}
	m.saveStateLocked()
	m.mu.Unlock()
	m.journal(vi, "entry", sym, fmt.Sprintf("BUY x%.0f @ $%.2f (signal $%.2f, slip %+.1fbps, stop $%.2f)",
		fq, price, signalPx, slip, stopPx))
	log.Printf("surger[%s]: ENTER %s x%.0f @ $%.2f (stop $%.2f)", VariantNames[vi], sym, fq, price, stopPx)
}

// ---------------- per-bar management (caller holds mu) ----------------

func (m *Manager) manageBarLocked(vi int, p *Position, close, high, low float64) {
	if high > p.HighPx {
		p.HighPx = high
	}
	if p.LowPx == 0 || low < p.LowPx {
		p.LowPx = low
	}
	if high > p.Peak {
		p.Peak = high
	}
	trail := trailWide
	if p.Peak >= p.EntryPrice*(1+tightenAt) {
		trail = trailTight
	}
	desired := round2(p.Peak * (1 - trail))
	if desired > p.StopPx*ratchetMinUp && desired < close {
		go m.ratchet(vi, p.Symbol, desired) // network I/O off the bar path
	}
	// software backup: bar closed at/under the stop and the exchange hasn't reported it
	if close <= p.StopPx {
		go m.executeExit(vi, p.Symbol, "stop_backup")
	}
}

// ratchet = confirmed-cancel stop replacement (the Bug-2 lesson: never fire-and-forget).
func (m *Manager) ratchet(vi int, sym string, newStop float64) {
	m.mu.Lock()
	p := m.books[vi].Open[sym]
	if p == nil || newStop <= p.StopPx*ratchetMinUp {
		m.mu.Unlock()
		return
	}
	stopID, qty, oldStop := p.StopID, p.Qty, p.StopPx
	m.mu.Unlock()

	if stopID != "" {
		_ = m.broker.Cancel(stopID)
		st, fq, ap := m.confirmCancel(stopID, 3*time.Second)
		if st == "filled" {
			m.recordExit(vi, sym, ap, "trail_stop", fq)
			return
		}
		if st == "" { // not confirmed — old stop still protects; retry next bar
			return
		}
	}
	sc := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[vi], sym, time.Now().UnixNano())
	sid, err := m.broker.StopSell(sym, qty, newStop, sc)
	if err != nil {
		// fall back to the prior level; if that too fails, flatten (never naked)
		sc2 := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[vi], sym, time.Now().UnixNano()+1)
		if sid2, err2 := m.broker.StopSell(sym, qty, oldStop, sc2); err2 == nil {
			m.setStop(vi, sym, sid2, oldStop)
		} else {
			m.journal(vi, "error", sym, "ratchet re-protect failed — flattening: "+err2.Error())
			m.setStop(vi, sym, "", oldStop)
			m.executeExit(vi, sym, "naked_flatten")
		}
		return
	}
	m.setStop(vi, sym, sid, newStop)
}

func (m *Manager) setStop(vi int, sym, sid string, px float64) {
	m.mu.Lock()
	if p := m.books[vi].Open[sym]; p != nil {
		p.StopID = sid
		p.StopPx = px
	}
	m.saveStateLocked()
	m.mu.Unlock()
}

// ---------------- exit ----------------

// executeExit: confirm-cancel our stop, market-sell exactly OUR quantity, settle terminal.
func (m *Manager) executeExit(vi int, sym, reason string) {
	m.mu.Lock()
	p := m.books[vi].Open[sym]
	if p == nil {
		m.mu.Unlock()
		return
	}
	stopID, qty := p.StopID, p.Qty
	m.mu.Unlock()

	if stopID != "" {
		_ = m.broker.Cancel(stopID)
		st, fq, ap := m.confirmCancel(stopID, 5*time.Second)
		if st == "filled" {
			m.recordExit(vi, sym, ap, "trail_stop", fq)
			return
		}
		if st == "" {
			m.journal(vi, "warn", sym, "stop not confirmed canceled — deferring exit (no double sell)")
			return
		}
	}
	coid := fmt.Sprintf("%s_%s_exit_%d", VariantCoid[vi], sym, time.Now().UnixNano())
	m.addInflight(inflight{Variant: vi, Symbol: sym, Coid: coid, Kind: "exit", Qty: qty, At: time.Now()})
	id, err := m.broker.MarketSell(sym, qty, coid)
	if err != nil {
		m.dropInflight(coid)
		// re-protect at the old level so we're not naked, retry on a later bar/tick
		sc := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[vi], sym, time.Now().UnixNano())
		if sid, serr := m.broker.StopSell(sym, qty, m.stopPxOf(vi, sym), sc); serr == nil {
			m.setStop(vi, sym, sid, m.stopPxOf(vi, sym))
		}
		m.journal(vi, "error", sym, "exit rejected ("+err.Error()+") — re-protected, will retry")
		return
	}
	fq, fp := m.settleOrder(id, 15*time.Second, false)
	m.dropInflight(coid)
	if fq+0.5 < qty { // partial exit: keep the remainder tracked + protected (no ghosts)
		rem := qty - fq
		m.journal(vi, "warn", sym, fmt.Sprintf("exit filled %.0f/%.0f — tracking remainder %.0f", fq, qty, rem))
		m.mu.Lock()
		if p2 := m.books[vi].Open[sym]; p2 != nil {
			p2.Qty = rem
			p2.StopID = ""
		}
		m.saveStateLocked()
		m.mu.Unlock()
		sc := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[vi], sym, time.Now().UnixNano())
		if sid, serr := m.broker.StopSell(sym, rem, m.stopPxOf(vi, sym), sc); serr == nil {
			m.setStop(vi, sym, sid, m.stopPxOf(vi, sym))
		}
		if fq >= 1 { // book the filled part as a partial trade
			m.recordPartial(vi, sym, fq, fp, reason)
		}
		return
	}
	m.recordExit(vi, sym, fp, reason, fq)
}

func (m *Manager) stopPxOf(vi int, sym string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.books[vi].Open[sym]; p != nil {
		return p.StopPx
	}
	return 0
}

func (m *Manager) recordPartial(vi int, sym string, qty, px float64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.books[vi].Open[sym]
	if p == nil {
		return
	}
	m.books[vi].Trades = append(m.books[vi].Trades, Trade{
		Variant: vi, Symbol: sym, Qty: qty, EntryPrice: p.EntryPrice, ExitPrice: px,
		PnL: (px - p.EntryPrice) * qty, Reason: reason + "_partial",
		OpenedAt: p.OpenedAt, ClosedAt: time.Now(),
		SignalPx: p.SignalPx, EntrySlip: p.EntrySlip, HighPx: p.HighPx, LowPx: p.LowPx,
	})
	m.saveStateLocked()
}

func (m *Manager) recordExit(vi int, sym string, px float64, reason string, qty float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.books[vi].Open[sym]
	if p == nil {
		return
	}
	if px <= 0 {
		px = p.StopPx
	}
	if qty <= 0 {
		qty = p.Qty
	}
	hi, lo := p.HighPx, p.LowPx
	if px > hi {
		hi = px
	}
	if lo == 0 || px < lo {
		lo = px
	}
	m.books[vi].Trades = append(m.books[vi].Trades, Trade{
		Variant: vi, Symbol: sym, Qty: qty, EntryPrice: p.EntryPrice, ExitPrice: px,
		PnL: (px - p.EntryPrice) * qty, Reason: reason,
		OpenedAt: p.OpenedAt, ClosedAt: time.Now(),
		SignalPx: p.SignalPx, EntrySlip: p.EntrySlip, HighPx: hi, LowPx: lo,
	})
	delete(m.books[vi].Open, sym)
	m.books[vi].cooldown[sym] = time.Now()
	m.saveStateLocked()
	log.Printf("surger[%s]: EXIT %s x%.0f $%.2f→$%.2f P&L $%.2f (%s)",
		VariantNames[vi], sym, qty, p.EntryPrice, px, (px-p.EntryPrice)*qty, reason)
	m.journalLocked(vi, "exit", sym, fmt.Sprintf("x%.0f $%.2f→$%.2f pnl $%.2f (%s)",
		qty, p.EntryPrice, px, (px-p.EntryPrice)*qty, reason))
}

// ---------------- upkeep tick ----------------

func (m *Manager) tick() {
	now := time.Now().In(m.etz)
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return
	}
	mins := now.Hour()*60 + now.Minute()
	if mins < 9*60+30 || mins > 16*60+1 {
		return
	}
	type check struct {
		vi     int
		sym    string
		stopID string
	}
	m.mu.Lock()
	var checks []check
	for vi := range m.books {
		for sym, p := range m.books[vi].Open {
			checks = append(checks, check{vi, sym, p.StopID})
		}
	}
	m.mu.Unlock()

	for _, ch := range checks {
		if ch.stopID != "" {
			if fq, ap, st, err := m.broker.Order(ch.stopID); err == nil && st == "filled" && fq > 0 {
				m.recordExit(ch.vi, ch.sym, ap, "trail_stop", fq)
				continue
			}
		} else {
			// naked (a failed re-protect earlier) — protect or flatten now
			m.executeExit(ch.vi, ch.sym, "naked_flatten")
			continue
		}
		if mins >= eodFlatMin {
			m.executeExit(ch.vi, ch.sym, "eod")
		}
	}
}

// ---------------- boot: settle in-flight, re-verify protection ----------------

func (m *Manager) settleInflightBoot() {
	m.mu.Lock()
	pend := make([]inflight, 0, len(m.inflight))
	for _, f := range m.inflight {
		pend = append(pend, f)
	}
	m.mu.Unlock()
	for _, f := range pend {
		if f.OrderID == "" { // never confirmed placed; nothing to settle
			m.dropInflight(f.Coid)
			continue
		}
		fq, ap, st, err := m.broker.Order(f.OrderID)
		if err != nil {
			continue // keep it; retried next boot
		}
		m.journal(f.Variant, "boot", f.Symbol, fmt.Sprintf("in-flight %s settled: %s fq=%.0f", f.Kind, st, fq))
		if f.Kind == "entry" && st == "filled" && fq > 0 {
			m.mu.Lock()
			if m.books[f.Variant].Open[f.Symbol] == nil {
				m.books[f.Variant].Open[f.Symbol] = &Position{
					Variant: f.Variant, Symbol: f.Symbol, Qty: fq, EntryPrice: ap,
					OpenedAt: f.At, Peak: ap, SignalPx: ap, HighPx: ap, LowPx: ap,
				}
			}
			m.saveStateLocked()
			m.mu.Unlock()
		}
		if f.Kind == "exit" && st == "filled" && fq > 0 {
			m.recordExit(f.Variant, f.Symbol, ap, "boot_settled_exit", fq)
		}
		m.dropInflight(f.Coid)
	}
}

// rehydrate re-verifies every persisted open position: stop filled while offline →
// record; stop missing/canceled → fresh stop (or flatten if it can't rest).
func (m *Manager) rehydrate() {
	m.mu.Lock()
	type item struct {
		vi  int
		sym string
		p   Position
	}
	var items []item
	for vi := range m.books {
		for sym, p := range m.books[vi].Open {
			items = append(items, item{vi, sym, *p})
		}
	}
	m.mu.Unlock()
	for _, it := range items {
		if it.p.StopID != "" {
			if fq, ap, st, err := m.broker.Order(it.p.StopID); err == nil {
				if st == "filled" && fq > 0 {
					m.recordExit(it.vi, it.sym, ap, "trail_stop_offline", fq)
					continue
				}
				if st == "new" || st == "accepted" || st == "held" {
					continue // still protected
				}
			}
		}
		sc := fmt.Sprintf("%s_%s_stop_%d", VariantCoid[it.vi], it.sym, time.Now().UnixNano())
		if sid, err := m.broker.StopSell(it.sym, it.p.Qty, it.p.StopPx, sc); err == nil {
			m.setStop(it.vi, it.sym, sid, it.p.StopPx)
			m.journal(it.vi, "boot", it.sym, "re-protected after restart")
		} else {
			m.journal(it.vi, "boot", it.sym, "re-protect failed — flattening: "+err.Error())
			m.executeExit(it.vi, it.sym, "boot_flatten")
		}
	}
	if n := len(items); n > 0 {
		log.Printf("surger: rehydrated %d position(s)", n)
	}
}

// ---------------- order settlement helpers ----------------

// settleOrder waits for a terminal state; if cancelIfLive and the deadline passes, the
// remainder is canceled and re-read so the book only ever records what actually filled.
func (m *Manager) settleOrder(id string, max time.Duration, cancelIfLive bool) (float64, float64) {
	fq, ap, terminal := m.awaitTerminal(id, max)
	if terminal || !cancelIfLive {
		return fq, ap
	}
	_ = m.broker.Cancel(id)
	fq, ap, _ = m.awaitTerminal(id, 4*time.Second)
	return fq, ap
}

func (m *Manager) awaitTerminal(id string, max time.Duration) (float64, float64, bool) {
	deadline := time.Now().Add(max)
	var fq, ap float64
	for time.Now().Before(deadline) {
		q, p, st, err := m.broker.Order(id)
		if err == nil {
			if q > 0 {
				fq, ap = q, p
			}
			switch st {
			case "filled", "canceled", "expired", "rejected", "replaced", "done_for_day":
				return fq, ap, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fq, ap, false
}

// confirmCancel polls a canceled order to a terminal state.
// Returns (state, filledQty, avgPrice): state "" = unconfirmed, "filled", or "canceled".
func (m *Manager) confirmCancel(id string, max time.Duration) (string, float64, float64) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if fq, ap, st, err := m.broker.Order(id); err == nil {
			switch st {
			case "canceled", "expired", "rejected", "replaced":
				return "canceled", fq, ap
			case "filled":
				return "filled", fq, ap
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", 0, 0
}

// ---------------- in-flight ledger / journal / state ----------------

func (m *Manager) addInflight(f inflight) {
	m.mu.Lock()
	m.inflight[f.Coid] = f
	m.saveStateLocked()
	m.mu.Unlock()
}

func (m *Manager) setInflightID(coid, id string) {
	m.mu.Lock()
	if f, ok := m.inflight[coid]; ok {
		f.OrderID = id
		m.inflight[coid] = f
	}
	m.saveStateLocked()
	m.mu.Unlock()
}

func (m *Manager) dropInflight(coid string) {
	m.mu.Lock()
	delete(m.inflight, coid)
	m.saveStateLocked()
	m.mu.Unlock()
}

func (m *Manager) journal(vi int, event, sym, note string) {
	m.mu.Lock()
	m.journalLocked(vi, event, sym, note)
	m.mu.Unlock()
}

func (m *Manager) journalLocked(vi int, event, sym, note string) {
	day := time.Now().In(m.etz).Format("2006-01-02")
	line, _ := json.Marshal(map[string]interface{}{
		"time": time.Now().In(m.etz).Format(time.RFC3339), "variant": VariantNames[vi],
		"event": event, "symbol": sym, "note": note,
	})
	f, err := os.OpenFile(filepath.Join(m.dataDir, day+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

type persisted struct {
	Books    [NumVariants]*book  `json:"books"`
	Inflight map[string]inflight `json:"inflight"`
}

func (m *Manager) statePath() string { return filepath.Join(m.dataDir, "state.json") }

func (m *Manager) loadState() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var st persisted
	if json.Unmarshal(b, &st) != nil {
		log.Printf("surger: state.json unreadable — starting fresh (in-flight ledger empty)")
		return
	}
	for i := range m.books {
		if st.Books[i] != nil {
			if st.Books[i].Open == nil {
				st.Books[i].Open = map[string]*Position{}
			}
			st.Books[i].cooldown = map[string]time.Time{}
			m.books[i] = st.Books[i]
		}
	}
	if st.Inflight != nil {
		m.inflight = st.Inflight
	}
}

// saveStateLocked writes atomically (temp+rename). Caller holds mu.
func (m *Manager) saveStateLocked() {
	b, err := json.MarshalIndent(persisted{Books: m.books, Inflight: m.inflight}, "", " ")
	if err != nil {
		return
	}
	tmp := m.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		_ = os.Rename(tmp, m.statePath())
	}
}

// ---------------- report ----------------

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func pick(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}

// Report is the /api/surger payload: three cleanly-separated variant scorecards.
func (m *Manager) Report() interface{} {
	day := time.Now().In(m.etz).Format("2006-01-02")
	m.mu.Lock()
	defer m.mu.Unlock()
	variants := make([]map[string]interface{}, 0, NumVariants)
	for vi := range m.books {
		b := m.books[vi]
		var realized, today float64
		wins := 0
		for _, t := range b.Trades {
			realized += t.PnL
			if t.PnL > 0 {
				wins++
			}
			if t.ClosedAt.In(m.etz).Format("2006-01-02") == day {
				today += t.PnL
			}
		}
		wr := 0.0
		if len(b.Trades) > 0 {
			wr = float64(wins) / float64(len(b.Trades)) * 100
		}
		open := make([]Position, 0, len(b.Open))
		var unreal float64
		for sym, p := range b.Open {
			if sr := m.series[sym]; sr != nil && len(sr.c) > 0 {
				unreal += (sr.c[len(sr.c)-1] - p.EntryPrice) * p.Qty
			}
			open = append(open, *p)
		}
		sort.Slice(open, func(i, j int) bool { return open[i].Symbol < open[j].Symbol })
		trades := b.Trades
		if len(trades) > 40 {
			trades = trades[len(trades)-40:]
		}
		variants = append(variants, map[string]interface{}{
			"name":           VariantNames[vi],
			"coid_prefix":    VariantCoid[vi] + "_",
			"realized_pnl":   round2(realized),
			"realized_today": round2(today),
			"unrealized_pnl": round2(unreal),
			"total_trades":   len(b.Trades),
			"win_rate":       round2(wr),
			"open":           open,
			"trades":         trades,
		})
	}
	return map[string]interface{}{
		"enabled":  m.Enabled(),
		"live":     m.live,
		"notional": m.notional,
		"slots":    m.slots,
		"universe": len(m.universe),
		"note":     "runs on the dip+rise paper account; srg*_ coids keep books separate; account day P&L is shared",
		"variants": variants,
	}
}
