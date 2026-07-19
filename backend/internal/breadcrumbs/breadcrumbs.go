// Package breadcrumbs is the generalized volatility-scalper paper desk: the SNDK 1-minute
// LightGBM pipeline, extended to the whole validated 22-name high-volatility basket, with a
// hard BUDGET tracker and a leak-proof position book reconciled against the broker every
// cycle. It runs on its OWN Alpaca paper account (PAPER_BREADCRUMBS_*) — strict one desk per
// account (a shared account lets desks liquidate each other's shares). It never touches the
// live real-money path.
//
// The pipeline, validated by walk-forward + a frozen out-of-time holdout
// (SNDK_VOLATILITY_PIPELINE_STUDY.md): 9 scale-free features → pooled LightGBM (retrained
// monthly) → entry gate (prob≥0.65 + Close>EMA100 + within 2σ of VWAP) → exit = 0.2%
// trailing stop with a profit-lock floored at the +0.57% target, hard stop −0.71%, EOD flat.
//
// The three hard-won invariants (a real 2026-07 ghost-share incident shaped them):
//  1. BUDGET: total deployed notional can never exceed the configured budget.
//  2. LEAK-PROOF: the book is reconciled against the broker's real positions every cycle —
//     account orphans are ADOPTED (+protected), book ghosts are recorded closed. The account
//     is the source of truth; state.json is only a hint.
//  3. NEVER NAKED: an exit cancels the exchange stop (confirmed) before selling, re-protects
//     on any failure, and flattens the FULL account quantity, not the book quantity.
package breadcrumbs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

// Position is one open long on the paper account, with the full exit-state it needs so a
// restart (rehydrate) can resume managing it deterministically.
type Position struct {
	Symbol      string    `json:"symbol"`
	Qty         float64   `json:"qty"`
	EntryPrice  float64   `json:"entry_price"`
	OpenedAt    time.Time `json:"opened_at"`
	TargetPrice float64   `json:"target_price"` // entry*(1+tp) — reaching it arms the trail
	StopLoss    float64   `json:"stop_loss"`    // entry*(1-sl) — hard stop (also the exchange stop)
	StopID      string    `json:"stop_id"`      // exchange-side protective stop order id
	Peak        float64   `json:"peak"`         // highest price seen since arming (trail anchor)
	Armed       bool      `json:"armed"`        // target reached → trailing active
	Locked      bool      `json:"locked"`       // profit locked: exchange stop moved up to (≥) the target
	Adopted     bool      `json:"adopted"`      // recovered via reconcile (no original context)
	Prob        float64   `json:"prob"`         // model probability at entry (for the report)
}

// Trade is a closed round-trip.
type Trade struct {
	Symbol     string    `json:"symbol"`
	Qty        float64   `json:"qty"`
	EntryPrice float64   `json:"entry_price"`
	ExitPrice  float64   `json:"exit_price"`
	PnL        float64   `json:"pnl"`
	Reason     string    `json:"reason"` // target/trail/stop_loss/catastrophic_stop/eod/reconcile_vanished/safety_exit
	OpenedAt   time.Time `json:"opened_at"`
	ClosedAt   time.Time `json:"closed_at"`
}

// Manager is the desk. All position/trade state is guarded by mu.
type Manager struct {
	mu       sync.RWMutex
	broker   *quant.Broker
	engine   *candles.Engine
	etz      *time.Location
	dataDir  string
	live     bool
	universe []string

	// Budget & sizing (the scale guardrails).
	budget   float64 // hard cap on total deployed notional (USD)
	notional float64 // per-trade slice (USD) → qty = floor(notional/price)
	maxSlots int     // max concurrent positions

	// Exit dials (must match the model's % labels).
	tpPct    float64 // target %, arms trail (default 0.0057)
	slPct    float64 // hard stop % (default 0.0071)
	trailPct float64 // trailing width % (default 0.002)
	lock     bool    // floor the trail at the target (profit-lock)

	open       map[string]*Position
	pending    map[string]float64 // symbol → reserved notional for an in-flight entry (not yet in open)
	trades     []Trade
	scanning   bool // guards against overlapping entry scans
	lastScan   int  // last entry-scan minute (dedupe per-minute boundary)
	lastRecon  time.Time
	ensureLive func(string)
	pyPath     string
	scriptPath string
	modelPath  string
	trainedTo  string // model meta trained_through (for the report)
}

type sig struct {
	Signal      bool    `json:"signal"`
	Probability float64 `json:"probability"`
	Close       float64 `json:"close"`
}

// New builds the desk. universe/budget/etc come from config; sensible validated defaults are
// applied for any zero value so a partial .env can't silently disable the guards.
func New(broker *quant.Broker, engine *candles.Engine, etz *time.Location, dataDir string,
	live bool, universe []string, budget, notional float64, maxSlots int,
	tpPct, slPct, trailPct float64, lock bool) *Manager {
	if etz == nil {
		etz = time.UTC
	}
	if budget <= 0 {
		budget = 200000
	}
	if notional <= 0 {
		notional = 2000
	}
	if maxSlots <= 0 {
		maxSlots = len(universe) // one slot per symbol by default
	}
	if tpPct <= 0 {
		tpPct = 0.0057
	}
	if slPct <= 0 {
		slPct = 0.0071
	}
	if trailPct <= 0 {
		trailPct = 0.002
	}
	m := &Manager{
		broker: broker, engine: engine, etz: etz,
		dataDir:  filepath.Join(dataDir, "breadcrumbs"),
		live:     live,
		universe: universe,
		budget:   budget, notional: notional, maxSlots: maxSlots,
		tpPct: tpPct, slPct: slPct, trailPct: trailPct, lock: lock,
		open:     map[string]*Position{},
		pending:  map[string]float64{},
		trades:   []Trade{},
		lastScan: -1,
	}
	// ml/ scripts live next to the backend working dir (../ml), matching the SNDK desk.
	m.pyPath = filepath.Join("..", "ml", ".venv", "Scripts", "python.exe")
	m.scriptPath = filepath.Join("..", "ml", "breadcrumbs_live_signals.py")
	m.modelPath = filepath.Join("..", "ml", "breadcrumbs_model.bin")
	_ = os.MkdirAll(m.dataDir, 0755)
	m.loadState()
	m.loadMeta()
	return m
}

func (m *Manager) SetEnsureLive(fn func(string)) { m.ensureLive = fn }

func (m *Manager) Enabled() bool { return m != nil && m.broker.Enabled() }

func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		log.Printf("breadcrumbs: disabled (no PAPER_BREADCRUMBS keys — strict one account per desk)")
		return
	}
	// Stream every basket symbol so the 1-min engine tracks it (entry scan reads its bars,
	// exits mark to its live price).
	if m.ensureLive != nil {
		for _, s := range m.universe {
			m.ensureLive(s)
		}
	}
	// Boot reconcile: adopt+protect anything already on the account (rehydrate after a
	// restart) BEFORE the loops start managing. reconcile removes vanished positions (and
	// cancels their orphaned stops) and adopts orphans, so afterwards every book position is
	// confirmed held on the account.
	m.reconcile()
	// Then re-protect every surviving position with exactly ONE fresh stop: a restart may have
	// left a stale StopID (crash mid-ratchet → naked) or a lost state.json may mean a
	// pre-existing exchange stop we don't track (→ a second, orphaned stop). CancelOpenOrders
	// + one fresh StopSell guarantees exactly one valid stop. Safe (positions are confirmed
	// held) and boot-only, so it never fights the live ratchet.
	m.rehydrateProtect()

	log.Printf("breadcrumbs: started (live=%v, universe=%d, budget=$%.0f, notional=$%.0f, slots=%d, exit=%.2f%%trail+%s@%.2f%%tgt/-%.2f%%stop)",
		m.live, len(m.universe), m.budget, m.notional, m.maxSlots,
		m.trailPct*100, lockLabel(m.lock), m.tpPct*100, m.slPct*100)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
}

func lockLabel(b bool) string {
	if b {
		return "lock"
	}
	return "nolock"
}

// rehydrateProtect re-places exactly one fresh protective stop for every held position after
// a restart. Only call once, at boot, AFTER reconcile has confirmed the book equals the
// account — never in the live loop (it would fight the ratcheting trail).
func (m *Manager) rehydrateProtect() {
	if !m.live {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pos := range m.open {
		m.reprotectLocked(pos)
	}
	if len(m.open) > 0 {
		m.saveStateLocked()
		log.Printf("breadcrumbs: rehydrated %d position(s) with fresh protective stops", len(m.open))
	}
}

func (m *Manager) tick() {
	now := time.Now().In(m.etz)
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return
	}
	mins := now.Hour()*60 + now.Minute()

	// Exit management + reconcile run every 5s through the session. reconcile makes ONE
	// batched /positions call that covers the WHOLE book (not one order per position), so the
	// full leak-proof + stop-fill detection runs at 5s regardless of how many stocks are held
	// — 12 calls/min, far under the rate limit even at hundreds of names.
	if mins >= 9*60+30 && mins <= 16*60+1 {
		m.manageExits(now)
		m.reconcile()
		m.lastRecon = now
	} else if time.Since(m.lastRecon) >= 60*time.Second {
		// Off-hours: light upkeep (catch an overnight adopt/rehydrate); nothing to manage.
		m.lastRecon = now
		m.reconcile()
	}
	// Entry scan on each new 1-min boundary within the entry window (no lunch skip — the
	// validated sim traded all RTH). Runs in the background so a ~1-2s Python call never
	// stalls exit checks.
	if mins >= 9*60+31 && mins <= 15*60+50 {
		m.mu.Lock()
		fresh := now.Minute() != m.lastScan && !m.scanning
		if fresh {
			m.lastScan = now.Minute()
			m.scanning = true
		}
		m.mu.Unlock()
		if fresh {
			go func() {
				defer func() { m.mu.Lock(); m.scanning = false; m.mu.Unlock() }()
				m.runEntryScan()
			}()
		}
	}
}

func (m *Manager) lastPrice(sym string) float64 {
	bars := m.engine.Snapshot(sym, 1)
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close
}

// deployedLocked is the total notional currently at work (book, marked at entry). Caller holds mu.
func (m *Manager) deployedLocked() float64 {
	var d float64
	for _, p := range m.open {
		d += p.Qty * p.EntryPrice
	}
	return d
}

// pendingNotionalLocked is the notional reserved by in-flight entries. Caller holds mu.
func (m *Manager) pendingNotionalLocked() float64 {
	var d float64
	for _, c := range m.pending {
		d += c
	}
	return d
}

// clearPending releases an in-flight entry's reservation.
func (m *Manager) clearPending(sym string) {
	m.mu.Lock()
	delete(m.pending, sym)
	m.mu.Unlock()
}

// ---------------- Entry ----------------

func (m *Manager) runEntryScan() {
	// Collect recent 1-min bars for every basket symbol the engine tracks (skip held ones).
	batch := map[string][]candles.Candle{}
	m.mu.RLock()
	full := len(m.open) >= m.maxSlots
	held := make(map[string]bool, len(m.open))
	for s := range m.open {
		held[s] = true
	}
	m.mu.RUnlock()
	if full {
		return // no free slot → don't even score
	}
	for _, s := range m.universe {
		if held[s] {
			continue
		}
		bars := m.engine.Snapshot(s, 1)
		if len(bars) < 100 {
			continue
		}
		// Send up to ~1000 recent 1-min bars: after the scorer RTH-filters (dropping
		// pre/after-market), this still leaves enough regular-hours history — reaching into
		// the prior session — for EMA-100 to be warm at the open, matching training.
		if len(bars) > 1000 {
			bars = bars[len(bars)-1000:]
		}
		batch[s] = bars
	}
	if len(batch) == 0 {
		return
	}

	batchFile := filepath.Join(m.dataDir, "batch_bars.json")
	outFile := filepath.Join(m.dataDir, "signals.json")
	bb, _ := json.Marshal(batch)
	if err := os.WriteFile(batchFile, bb, 0644); err != nil {
		log.Printf("breadcrumbs: write batch failed: %v", err)
		return
	}
	_ = os.Remove(outFile)
	cmd := exec.Command(m.pyPath, m.scriptPath, "--batch", batchFile, "--out", outFile, "--model", m.modelPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("breadcrumbs: scorer error: %v | %s", err, string(out))
		return
	}
	rb, err := os.ReadFile(outFile)
	if err != nil {
		log.Printf("breadcrumbs: signals file not written: %v", err)
		return
	}
	var signals map[string]sig
	if err := json.Unmarshal(rb, &signals); err != nil {
		log.Printf("breadcrumbs: parse signals: %v", err)
		return
	}

	// Rank buy signals by probability and take them in order until slots/budget run out.
	type cand struct {
		sym string
		s   sig
	}
	var cands []cand
	for s, sg := range signals {
		if sg.Signal && sg.Close > 0 {
			cands = append(cands, cand{s, sg})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].s.Probability > cands[j].s.Probability })
	for _, c := range cands {
		m.tryEnter(c.sym, c.s.Close, c.s.Probability)
	}
}

// tryEnter opens a position if a slot is free, the symbol isn't already held, the budget has
// room, and buying power covers it. Entries are placed SEQUENTIALLY (one scan goroutine) so
// the budget/slot check always sees the up-to-date book.
func (m *Manager) tryEnter(sym string, sigClose, prob float64) {
	m.mu.Lock()
	if _, held := m.open[sym]; held {
		m.mu.Unlock()
		return
	}
	if _, pend := m.pending[sym]; pend {
		m.mu.Unlock()
		return
	}
	// Slots + budget count BOTH open positions and in-flight (pending) entries so concurrent
	// or in-progress buys can't overshoot either guard.
	if len(m.open)+len(m.pending) >= m.maxSlots {
		m.mu.Unlock()
		return
	}
	price := sigClose
	qty := math.Floor(m.notional / price)
	if qty < 1 {
		m.mu.Unlock()
		return
	}
	cost := qty * price
	if m.deployedLocked()+m.pendingNotionalLocked()+cost > m.budget {
		used := m.deployedLocked() + m.pendingNotionalLocked()
		m.mu.Unlock()
		log.Printf("breadcrumbs: %s skipped — budget cap ($%.0f used + $%.0f > $%.0f)",
			sym, used, cost, m.budget)
		return
	}
	// Reserve the pending slot BEFORE releasing the lock so the reconcile loop won't adopt
	// (and double-protect) this position while its buy is in flight.
	m.pending[sym] = cost
	m.mu.Unlock()
	defer m.clearPending(sym)

	if !m.live {
		log.Printf("breadcrumbs: [SHADOW] would BUY %s x%.0f @ $%.2f (prob %.2f)", sym, qty, price, prob)
		return
	}

	// Real buying-power guard (margin/settlement realities the budget cap can't see).
	acc, err := m.broker.Account()
	if err == nil && acc.BuyingPower < cost {
		q := math.Floor(acc.BuyingPower / price)
		if q < 1 {
			log.Printf("breadcrumbs: %s skipped — buying power $%.0f < $%.0f", sym, acc.BuyingPower, cost)
			return
		}
		qty = q
	}

	coid := fmt.Sprintf("bc_entry_%s_%d", sym, time.Now().UnixNano())
	id, err := m.broker.MarketBuy(sym, qty, coid)
	if err != nil {
		log.Printf("breadcrumbs: %s entry order failed: %v", sym, err)
		return
	}
	// Wait for the fill so the stop is sized to the ACTUAL position and P&L matches the account.
	if fq, fp := m.awaitFill(id, 12*time.Second); fq > 0 {
		qty = fq
		if fp > 0 {
			price = fp
		}
	}

	tp := price * (1 + m.tpPct)
	sl := price * (1 - m.slPct)
	stopCoid := fmt.Sprintf("bc_stop_%s_%d", sym, time.Now().UnixNano())
	stopID, sErr := m.broker.StopSell(sym, qty, sl, stopCoid)
	if sErr != nil {
		// Never hold naked: flatten immediately if the protective stop won't place.
		log.Printf("breadcrumbs: %s protective stop failed (%v) — flattening for safety", sym, sErr)
		flat := fmt.Sprintf("bc_safety_%s_%d", sym, time.Now().UnixNano())
		_, _ = m.broker.MarketSell(sym, qty, flat)
		return
	}

	m.mu.Lock()
	m.open[sym] = &Position{
		Symbol: sym, Qty: qty, EntryPrice: price, OpenedAt: time.Now(),
		TargetPrice: tp, StopLoss: sl, StopID: stopID, Peak: price, Prob: prob,
	}
	m.saveStateLocked()
	m.mu.Unlock()
	log.Printf("breadcrumbs: ENTER %s x%.0f @ $%.2f (prob %.2f | tgt $%.2f | stop $%.2f)",
		sym, qty, price, prob, tp, sl)
}

// ---------------- Exit ----------------

// manageExits drives the lock-then-trail exit for every open position:
//  1. hard stop at −slPct rests on the exchange from entry (protects the pre-target leg).
//  2. when price reaches the +tpPct TARGET, the profit is LOCKED — the exchange stop is
//     ratcheted UP to the target so the gain is protected exchange-side even if this
//     process dies. THEN the trailing stop begins.
//  3. as price makes new highs, the exchange stop ratchets up under the trailPct trail
//     (never below the locked target), throttled so it doesn't churn orders every tick.
//
// The exchange stop is the real exit (sub-second, survives a restart); the software checks
// below are a same-tick backup, and reconcile is the 30s safety net. EOD flattens at 15:59.
func (m *Manager) manageExits(now time.Time) {
	m.mu.RLock()
	syms := make([]string, 0, len(m.open))
	for s := range m.open {
		syms = append(syms, s)
	}
	m.mu.RUnlock()

	mins := now.Hour()*60 + now.Minute()
	for _, sym := range syms {
		price := m.lastPrice(sym)
		if price <= 0 {
			continue
		}
		m.mu.Lock()
		pos := m.open[sym]
		if pos == nil {
			m.mu.Unlock()
			continue
		}
		// (2) Arm + LOCK: first time price tags the target, move the exchange stop up to the
		// target so the profit can't be given back below it, then the trail takes over.
		if !pos.Armed && price >= pos.TargetPrice {
			pos.Armed = true
			pos.Peak = math.Max(price, pos.TargetPrice)
			if m.lock {
				m.ratchetStopLocked(pos, pos.TargetPrice)
				pos.Locked = true
			}
			m.saveStateLocked()
			log.Printf("breadcrumbs: %s LOCKED profit @ target $%.2f — trailing begins", sym, pos.TargetPrice)
		}
		// (3) Trail: ratchet the exchange stop up under the rising peak, floored at the target.
		// Move it UP only, only when meaningfully higher (≥0.15%, to bound order churn), and
		// only while it stays BELOW the current price (a resting stop must sit below market —
		// otherwise it's marketable and fires instantly). When price has actually fallen to
		// the trail, the software-backup exit below handles it instead.
		if pos.Armed {
			if price > pos.Peak {
				pos.Peak = price
			}
			desired := pos.Peak * (1 - m.trailPct)
			if m.lock && desired < pos.TargetPrice {
				desired = pos.TargetPrice
			}
			if desired > pos.StopLoss*(1+0.0015) && desired < price {
				m.ratchetStopLocked(pos, desired)
				// Deliberately NOT persisted per ratchet (that rewrites the whole trades
				// history every few seconds × every position). On restart, rehydrateProtect
				// re-places a fresh stop at the last-saved level and the trail re-ratchets from
				// the current peak — safe, and it removes the write amplification at scale.
			}
		}
		reason := reasonFor(price, pos)
		stop := pos.StopLoss
		m.mu.Unlock()

		// Same-tick backup: if price already sits at/below the (possibly ratcheted) stop and
		// the exchange hasn't filled it yet, flatten now.
		if price <= stop {
			m.executeExit(sym, price, reason)
			continue
		}
		if mins >= 15*60+59 { // EOD flat
			m.executeExit(sym, price, "eod")
			continue
		}
	}
}

// reasonFor labels an exit by where it lands relative to entry (armed profit exits = trail).
func reasonFor(price float64, pos *Position) string {
	if price <= pos.EntryPrice {
		return "stop_loss"
	}
	return "trail"
}

// ratchetStopLocked replaces the exchange protective stop with one at newStop (only ever
// called to move it UP). Cancels the old stop first, then places the new one; on failure it
// re-places at the prior level so the position is never left naked. Caller holds mu.
func (m *Manager) ratchetStopLocked(pos *Position, newStop float64) {
	newStop = round2(newStop)
	if !m.live {
		pos.StopLoss = newStop
		return
	}
	if pos.StopID != "" {
		_ = m.broker.Cancel(pos.StopID)
	}
	sc := fmt.Sprintf("bc_stop_%s_%d", pos.Symbol, time.Now().UnixNano())
	if sid, err := m.broker.StopSell(pos.Symbol, pos.Qty, newStop, sc); err == nil {
		pos.StopID = sid
		pos.StopLoss = newStop
		return
	} else {
		log.Printf("breadcrumbs: %s ratchet stop failed: %v — re-protecting at $%.2f", pos.Symbol, err, pos.StopLoss)
	}
	sc2 := fmt.Sprintf("bc_stop_%s_%d", pos.Symbol, time.Now().UnixNano()+1)
	if sid, err := m.broker.StopSell(pos.Symbol, pos.Qty, pos.StopLoss, sc2); err == nil {
		pos.StopID = sid
	} else {
		pos.StopID = ""
		log.Printf("breadcrumbs: CRITICAL %s left with NO stop after ratchet failure: %v", pos.Symbol, err)
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// executeExit cancels the exchange stop (confirmed) then market-sells the FULL account
// quantity. Mirrors the SNDK desk's hardening: if the stop already filled, record that; if
// the sell fails after canceling, re-protect immediately so the position is never naked.
func (m *Manager) executeExit(sym string, price float64, reason string) {
	m.mu.RLock()
	pos := m.open[sym]
	m.mu.RUnlock()
	if pos == nil {
		return
	}

	if pos.StopID != "" {
		_ = m.broker.Cancel(pos.StopID)
		confirmed := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fq, ap, st, err := m.broker.Order(pos.StopID); err == nil {
				if st == "canceled" || st == "expired" || st == "rejected" || st == "replaced" {
					confirmed = true
					break
				}
				if st == "filled" && fq > 0 {
					m.recordExit(sym, ap, "catastrophic_stop")
					return
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
		if !confirmed {
			log.Printf("breadcrumbs: %s stop not confirmed canceled — deferring exit to avoid a double sell", sym)
			return
		}
	}

	// Flatten the FULL account quantity, not the book qty — this is the leak-proofing that
	// closes out any drift (partial fills, adopted orphans) in one shot.
	qty := pos.Qty
	if aq, err := m.broker.PositionQty(sym); err == nil && aq > 0 {
		qty = aq
	}
	coid := fmt.Sprintf("bc_exit_%s_%d", sym, time.Now().UnixNano())
	id, err := m.broker.MarketSell(sym, qty, coid)
	if err != nil {
		log.Printf("breadcrumbs: %s exit failed (%v) — re-protecting", sym, err)
		sc := fmt.Sprintf("bc_stop_%s_%d", sym, time.Now().UnixNano())
		if sid, serr := m.broker.StopSell(sym, qty, pos.StopLoss, sc); serr == nil {
			m.mu.Lock()
			if m.open[sym] != nil {
				m.open[sym].StopID = sid
			}
			m.saveStateLocked()
			m.mu.Unlock()
		} else {
			log.Printf("breadcrumbs: CRITICAL %s held with NO stop, re-protect failed: %v", sym, serr)
		}
		return
	}
	if _, ap := m.awaitFill(id, 12*time.Second); ap > 0 {
		price = ap
	}
	m.recordExit(sym, price, reason)
}

func (m *Manager) recordExit(sym string, price float64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pos := m.open[sym]
	if pos == nil {
		return
	}
	m.recordExitLocked(pos, price, reason)
}

func (m *Manager) recordExitLocked(pos *Position, price float64, reason string) {
	pnl := (price - pos.EntryPrice) * pos.Qty
	m.trades = append(m.trades, Trade{
		Symbol: pos.Symbol, Qty: pos.Qty, EntryPrice: pos.EntryPrice, ExitPrice: price,
		PnL: pnl, Reason: reason, OpenedAt: pos.OpenedAt, ClosedAt: time.Now(),
	})
	delete(m.open, pos.Symbol)
	m.saveStateLocked()
	log.Printf("breadcrumbs: EXIT %s x%.0f  $%.2f→$%.2f  P&L $%.2f (%s)",
		pos.Symbol, pos.Qty, pos.EntryPrice, price, pnl, reason)
}

// ---------------- Reconcile (the leak-proofing) ----------------

// reconcile makes the book agree with the broker's real positions. The dedicated account
// means every position on it is ours: adopt+protect account orphans (the "placed a lot of
// stocks and forgot to track them" failure mode), record book ghosts as closed, and correct
// any qty drift to the account truth.
func (m *Manager) reconcile() {
	if !m.Enabled() || !m.live {
		return
	}
	positions, err := m.broker.Positions()
	if err != nil {
		return // transient; try again next cycle (never act on a failed read)
	}
	acct := make(map[string]quant.BrokerPosition, len(positions))
	for _, p := range positions {
		acct[p.Symbol] = p
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	changed := false // only persist when reconcile actually mutates the book (not every 5s)

	// 1) Book positions the account no longer holds → the exchange stop fired (or a manual
	// close). Record it closed. Prefer the stop order's REAL fill price + an inferred reason;
	// fall back to the last mark. This is also the scale-safe catastrophic-stop detector: one
	// order lookup only for the few positions that actually vanished this cycle.
	for sym, pos := range m.open {
		if _, ok := acct[sym]; !ok {
			if time.Since(pos.OpenedAt) < 90*time.Second {
				continue // don't race a just-submitted entry that hasn't shown up yet
			}
			px := m.lastPrice(sym)
			reason := "reconcile_vanished"
			if pos.StopID != "" {
				if fq, ap, st, err := m.broker.Order(pos.StopID); err == nil && fq > 0 && st == "filled" {
					px = ap
					reason = reasonFor(ap, pos)
				}
			}
			if px <= 0 {
				px = pos.EntryPrice
			}
			// If the stop did NOT fill (manual/external close), it is still resting on the
			// exchange — cancel it so it can't later fire into a short. No-op if it already filled.
			if reason == "reconcile_vanished" {
				_ = m.broker.CancelOpenOrders(sym)
			}
			log.Printf("breadcrumbs: RECONCILE %s closed on the account @ $%.2f (%s) — recording", sym, px, reason)
			m.recordExitLocked(pos, px, reason)
			changed = true
		}
	}

	// 2) Account positions the book doesn't know about → orphan. Adopt + protect.
	for sym, ap := range acct {
		if ap.Qty <= 0 {
			continue
		}
		if _, pend := m.pending[sym]; pend {
			continue // an entry for this symbol is in flight — tryEnter owns it, don't adopt
		}
		pos, known := m.open[sym]
		if !known {
			log.Printf("breadcrumbs: RECONCILE adopting untracked account position %s x%.0f @ $%.2f", sym, ap.Qty, ap.AvgEntry)
			m.adoptLocked(sym, ap)
			changed = true
			continue
		}
		// 3) Known but qty drifted → trust the account. Resize the protective stop.
		if math.Abs(ap.Qty-pos.Qty) >= 1 {
			log.Printf("breadcrumbs: RECONCILE %s qty drift book=%.0f account=%.0f — correcting to account", sym, pos.Qty, ap.Qty)
			pos.Qty = ap.Qty
			m.reprotectLocked(pos)
			changed = true
		}
	}
	// Persist only when the book actually changed — not on every 5s pass (which would rewrite
	// the full trades history continuously). Exit records already persist themselves.
	if changed {
		m.saveStateLocked()
	}
}

// adoptLocked wraps an untracked account position in a fresh Position with percentage
// target/stop derived from its average entry, and places a protective exchange stop. Caller
// holds mu.
func (m *Manager) adoptLocked(sym string, ap quant.BrokerPosition) {
	pos := &Position{
		Symbol: sym, Qty: ap.Qty, EntryPrice: ap.AvgEntry, OpenedAt: time.Now(),
		TargetPrice: ap.AvgEntry * (1 + m.tpPct), StopLoss: ap.AvgEntry * (1 - m.slPct),
		Peak: ap.AvgEntry, Adopted: true,
	}
	m.open[sym] = pos
	m.reprotectLocked(pos)
}

// reprotectLocked (re)places the exchange-side protective stop for a position. It cancels
// ALL resting orders for the symbol first (not just the tracked StopID) so a stale or
// orphaned stop from a prior crash/restart can't linger as a second, dangerous order. Caller
// holds mu.
func (m *Manager) reprotectLocked(pos *Position) {
	_ = m.broker.CancelOpenOrders(pos.Symbol)
	sc := fmt.Sprintf("bc_stop_%s_%d", pos.Symbol, time.Now().UnixNano())
	if sid, err := m.broker.StopSell(pos.Symbol, pos.Qty, pos.StopLoss, sc); err == nil {
		pos.StopID = sid
	} else {
		log.Printf("breadcrumbs: %s re-protect failed: %v", pos.Symbol, err)
		pos.StopID = ""
	}
}

// ---------------- Helpers / state / report ----------------

func (m *Manager) awaitFill(id string, max time.Duration) (float64, float64) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		fq, ap, status, err := m.broker.Order(id)
		if err == nil && fq > 0 && (status == "filled" || status == "partially_filled") {
			return fq, ap
		}
		time.Sleep(700 * time.Millisecond)
	}
	return 0, 0
}

// Report is the /api/breadcrumbs payload. Budget/exposure are first-class so the page can
// show the desk is inside its cap at a glance.
func (m *Manager) Report() interface{} {
	acc, err := m.broker.Account()
	cash, equity, bp, dayPnL := 0.0, 0.0, 0.0, 0.0
	if err == nil {
		cash, equity, bp, dayPnL = acc.Cash, acc.Equity, acc.BuyingPower, acc.DayPnL()
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var realized float64
	wins := 0
	for _, t := range m.trades {
		realized += t.PnL
		if t.PnL > 0 {
			wins++
		}
	}
	winRate := 0.0
	if len(m.trades) > 0 {
		winRate = float64(wins) / float64(len(m.trades)) * 100
	}

	deployed := 0.0
	var unrealized float64
	positions := make([]Position, 0, len(m.open))
	for _, p := range m.open {
		deployed += p.Qty * p.EntryPrice
		if px := m.lastPrice(p.Symbol); px > 0 {
			unrealized += (px - p.EntryPrice) * p.Qty
		}
		positions = append(positions, *p)
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Symbol < positions[j].Symbol })

	return map[string]interface{}{
		"live":            m.live,
		"budget":          m.budget,
		"deployed":        deployed,
		"budget_free":     m.budget - deployed,
		"notional":        m.notional,
		"max_slots":       m.maxSlots,
		"open_count":      len(m.open),
		"universe_size":   len(m.universe),
		"universe":        m.universe,
		"model_trained":   m.trainedTo,
		"cash":            cash,
		"equity":          equity,
		"buying_power":    bp,
		"account_day_pnl": dayPnL, // Alpaca's own day P&L (equity − prior close): broker-level truth
		"realized_pnl":    realized,
		"unrealized_pnl":  unrealized,
		"total_trades":    len(m.trades),
		"win_rate":        winRate,
		"exit":            map[string]interface{}{"tp_pct": m.tpPct, "sl_pct": m.slPct, "trail_pct": m.trailPct, "lock": m.lock},
		"positions":       positions,
		"trades":          m.trades,
	}
}

func (m *Manager) statePath() string { return filepath.Join(m.dataDir, "state.json") }

type persisted struct {
	Open   map[string]*Position `json:"open"`
	Trades []Trade              `json:"trades"`
}

func (m *Manager) loadState() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var st persisted
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("breadcrumbs: state.json unreadable (%v) — starting from broker truth via reconcile", err)
		return
	}
	if st.Open != nil {
		m.open = st.Open
	}
	m.trades = st.Trades
}

// saveStateLocked writes state atomically (temp + rename) so a crash mid-write can't corrupt
// the book. Caller holds mu.
func (m *Manager) saveStateLocked() {
	b, err := json.MarshalIndent(persisted{Open: m.open, Trades: m.trades}, "", "  ")
	if err != nil {
		return
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		log.Printf("breadcrumbs: state write failed: %v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath())
}

// loadMeta reads the model's trained_through date for the report (best-effort).
func (m *Manager) loadMeta() {
	b, err := os.ReadFile(filepath.Join("..", "ml", "breadcrumbs_meta.json"))
	if err != nil {
		return
	}
	var meta struct {
		TrainedThrough string `json:"trained_through"`
	}
	if json.Unmarshal(b, &meta) == nil {
		m.trainedTo = meta.TrainedThrough
	}
}

// ---------------- Monthly rolling retrain ----------------

// StartRetrain runs the pooled trainer on a rolling monthly cadence with a boot catch-up, so
// the model walks forward automatically. The trainer refits on the trailing ~1 month and
// overwrites breadcrumbs_model.bin; the scorer reloads the .bin on the next scan. Fully
// hands-off. Retraining uses HISTORICAL data (works any time of day) but is scheduled after
// the close to avoid competing with live scans for the Python venv.
func (m *Manager) StartRetrain(ctx context.Context) {
	go func() {
		if m.retrainDue() {
			log.Printf("breadcrumbs: model missing/stale (through %q) — training on boot", m.trainedTo)
			m.retrain()
		}
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now().In(m.etz)
				weekday := now.Weekday() != time.Saturday && now.Weekday() != time.Sunday
				if now.Hour() >= 17 && weekday && m.retrainDue() {
					log.Printf("breadcrumbs: monthly rolling retrain (model through %q)", m.trainedTo)
					m.retrain()
				}
			}
		}
	}()
}

// retrainDue is true when there's no model, or the model's training month is behind the
// current month (the rolling-monthly trigger), or it's simply gone stale (>35 days).
func (m *Manager) retrainDue() bool {
	if _, err := os.Stat(m.modelPath); err != nil {
		return true
	}
	if m.trainedTo == "" {
		return true
	}
	t, err := time.Parse("2006-01-02", m.trainedTo)
	if err != nil {
		return true
	}
	now := time.Now().In(m.etz)
	if t.Year() != now.Year() || t.Month() != now.Month() {
		return true // a new calendar month began → roll the model forward
	}
	return now.Sub(t) > 35*24*time.Hour
}

// retrain execs the pooled trainer on the desk's own universe and reloads the model meta.
func (m *Manager) retrain() {
	script := filepath.Join("..", "ml", "train_breadcrumbs_model.py")
	cmd := exec.Command(m.pyPath, script, "--days", "35", "--symbols", strings.Join(m.universe, ","))
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		log.Printf("breadcrumbs: retrain FAILED: %v | %s", err, tail)
		return
	}
	m.loadMeta()
	log.Printf("breadcrumbs: retrain complete — model now through %q", m.trainedTo)
}
