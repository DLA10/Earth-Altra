package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// exitGraceMin is the MECHANICAL grace period (minutes): Agent 3 is not consulted at all
// during a position's first N minutes, so the entry's own deterministic plan (exchange
// stop, trailing floor, target, max hold, EOD flatten — all of which keep running from
// second zero) gets a fair chance before the LLM may cut it. Added 2026-07-16 after the
// exit audit: 7 of 9 rise exits were LLM-cut within 4 minutes (two within 15 seconds)
// for being pennies below entry — noise read as "structure broken" — on a strategy
// designed for a 40-minute window. Original behavior: no grace (Agent 3 consulted from
// the first tick). Env: QUANT_EXIT_GRACE_MIN (0 restores the original behavior).
var exitGraceMin = func() int {
	if v := strings.TrimSpace(os.Getenv("QUANT_EXIT_GRACE_MIN")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		log.Printf("[quant] ignoring invalid QUANT_EXIT_GRACE_MIN=%q (using 10)", v)
	}
	return 10
}()

func envFloatQ(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
		log.Printf("[quant] ignoring invalid %s=%q (using %v)", key, v, def)
	}
	return def
}

// Mechanical exit rails (2026-07-16, THROUGHPUT_MODE.md). All expressed in units of the
// position's OWN planned risk R (entry − original stop; trailing-stop distance when no
// fixed stop), so they scale with how the trade was sized. Set any to 0 to disable it.
var (
	// D — breakeven ratchet: once price reaches entry + beRatchetR×R, the stop moves to
	// entry. From then on the worst case is $0: a winner can no longer become a loser.
	beRatchetR = envFloatQ("QUANT_BREAKEVEN_R", 0.5)
	// E — grace checkpoints: one look at grace/2 (strict) and one at grace end (normal).
	// Exit only if the loss exceeds the threshold AND price is below session VWAP —
	// two independent conditions so a mere wiggle can't trip it.
	chkHalfR = envFloatQ("QUANT_CHK_HALF_R", 0.75)
	chkFullR = envFloatQ("QUANT_CHK_FULL_R", 0.5)
	// Noise floor: after grace, an Agent 3 exit_now on a LOSING position is honored only
	// when the loss is ≥ exitNoiseR×R. Profit-taking exits always pass. (2026-07-16
	// audit: the LLM cut positions 0.06% red — pennies — as "structure broken".)
	exitNoiseR = envFloatQ("QUANT_EXIT_NOISE_R", 0.25)
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
	source      string  // which pipeline opened it: "dip" | "signal" | "rise" | "rehydrated"
	strategy    string  // detector that opened it (signal pipeline) or "dip"
	target      float64 // original take-profit the entry set (0 = none, e.g. dip trades)
	origStop    float64 // original stop the entry set (0 = none)
	trailPct    float64 // per-entry trailing-stop % (0 = manager default)
	maxHoldMin  int     // deterministic time exit in minutes (0 = none)

	beDone  bool // breakeven ratchet already applied (or stop already ≥ entry)
	chkHalf bool // mid-grace checkpoint (grace/2) already evaluated
	chkFull bool // grace-end checkpoint already evaluated
}

// EntryContext carries what a position was opened WITH, so the exit agent can manage it
// knowing the plan (strategy personality + original target) instead of blind. Target/Stop
// are 0 when the pipeline set none (dip trades ride a trailing stop, no fixed target).
// TrailPct/MaxHoldMin let a pipeline run tighter, time-boxed exits (the rise watcher's
// short bounces); 0 keeps the manager defaults, so existing pipelines are unaffected.
type EntryContext struct {
	Source     string
	Strategy   string
	Target     float64
	Stop       float64
	TrailPct   float64
	MaxHoldMin int
}

// trailFor is the trailing-stop percent for one position (its own, else the desk default).
func (m *Manager) trailFor(pos *managedPos) float64 {
	if pos != nil && pos.trailPct > 0 {
		return pos.trailPct
	}
	return m.trailPct
}

// Manager owns the live position lifecycle: it places the entry, the deterministic trailing-stop
// floor (so the position is protected sub-second on Alpaca regardless of Agent 3), then runs the
// Agent 3 exit loop, executes its verbs as real orders, and releases capital when the position
// closes. The floor guarantees no position is ever left unmanaged.
type Manager struct {
	eng          *Engine
	alloc        *Allocator // THIS desk's capital pot (dip+rise and signal desks each have their own)
	broker       *Broker
	agent3       *Agent3
	trailPct     float64
	overnightCap float64 // max position VALUE allowed past the close (0 = flatten all)

	// OnClosed, when set, receives an APPROXIMATE realized P&L for every closed position
	// (marked to the last engine price at close detection). Feeds the daily loss-cap
	// tracker; the authoritative P&L remains the broker reconstruction.
	OnClosed func(sym string, approxPNL float64)

	// ensureLive, when set, subscribes a symbol's trades/quotes on the SIP stream so the
	// UI's open-position P&L ticks sub-second. Signal-universe symbols otherwise ride the
	// bar channel only (1-min updates) — this is why quant positions looked frozen on the
	// page while the backend was fine. Nil-safe; display-path only.
	ensureLive func(string)

	mu        sync.Mutex
	open      map[string]*managedPos
	keeperDay string // ET day the overnight keeper was chosen for
	keeperSym string // the single position allowed to hold overnight ("" = none)
}

// NewManager builds a position manager bound to ONE desk: its allocator (capital pot)
// and its broker (paper account). Two desks must never share either.
func NewManager(eng *Engine, alloc *Allocator, broker *Broker, agent3 *Agent3, trailPct, overnightCap float64) *Manager {
	if trailPct <= 0 {
		trailPct = 1.5
	}
	if alloc == nil && eng != nil {
		alloc = eng.alloc // legacy single-desk wiring
	}
	return &Manager{eng: eng, alloc: alloc, broker: broker, agent3: agent3, trailPct: trailPct,
		overnightCap: overnightCap, open: map[string]*managedPos{}}
}

// SetEnsureLive wires the on-demand streaming activation (sub-second position P&L in
// the UI) and applies it to any ALREADY-open positions — rehydration runs before the
// HTTP server (and this hook) exists, so survivors would otherwise miss streaming.
func (m *Manager) SetEnsureLive(fn func(string)) {
	m.ensureLive = fn
	if fn == nil {
		return
	}
	for _, s := range m.OpenSymbols() {
		fn(s)
	}
}

func (m *Manager) markLive(sym string) {
	if m.ensureLive != nil {
		m.ensureLive(sym)
	}
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

// Open executes an approved dip buy (the dipwatch pipeline's entry point). Dip trades ride
// the trailing stop with no fixed target, so the entry context carries only the source.
func (m *Manager) Open(ctx context.Context, de DipEvent, conf, dollars float64) {
	m.OpenPosition(ctx, de.Symbol, conf, dollars, EntryContext{Source: "dip", Strategy: "dip"})
}

// OpenPosition executes an approved buy for any pipeline (dip or signal engine): market
// entry → confirm fill → place the trailing-stop floor → register → run the Agent 3 loop.
// Releases the reserved capital on any pre-registration failure. The EntryContext tags the
// pipeline (so per-pipeline P&L can be measured) and records the entry plan (strategy +
// original target/stop) so the exit agent can manage the trade with knowledge of its goal.
func (m *Manager) OpenPosition(ctx context.Context, sym string, conf, dollars float64, ec EntryContext) {
	registered := false
	defer func() {
		if !registered {
			m.alloc.Release(sym) // never opened → give the capital back
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
	trail := m.trailPct
	if ec.TrailPct > 0 {
		trail = ec.TrailPct
	}
	stopCoid := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, sym, time.Now().UnixNano())
	stopID, serr := m.broker.TrailingStopSell(sym, fq, trail, stopCoid)
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
		stopPrice: round2(ap * (1 - trail/100)), conf: conf,
		source: ec.Source, strategy: ec.Strategy, target: ec.Target, origStop: ec.Stop,
		trailPct: ec.TrailPct, maxHoldMin: ec.MaxHoldMin}
	m.mu.Lock()
	m.open[sym] = pos
	m.mu.Unlock()
	registered = true
	m.markLive(sym)
	m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "order", Symbol: sym,
		Note: fmt.Sprintf("entry %.0f @ $%.2f; trailing stop %.1f%% placed", fq, ap, trail)})
	log.Printf("[quant] ENTER %s %.0f @ $%.2f (conf %.2f); trailing stop %.1f%%", sym, fq, ap, conf, trail)
	m.manage(ctx, pos)
}

// openOrderStatuses are Alpaca statuses under which a protective order is still working.
var openOrderStatuses = map[string]bool{
	"new": true, "accepted": true, "held": true, "partially_filled": true,
	"pending_new": true, "accepted_for_bidding": true, "calculated": true,
}

// foreignDeskPrefixes are the client-order-id prefixes of the OTHER paper desks. A
// position whose most recent filled entry buy carries one of these was opened by a
// sibling desk sharing this account — it is NOT ours to adopt, re-stop, or flatten.
// (2026-07-13/14 incident: Rehydrate adopted RIDP's reverter positions, canceled their
// exchange stops as "wrong-size", and Agent 3 sold them minutes later.)
// "srg" covers srg1_/srg2_/srg3_ — the SURGER v2 lab, which SHARES the dip+rise
// account by design (its books stay separate via these prefixes).
var foreignDeskPrefixes = []string{"ridp_", "rbt_", "sndk_", "srg"}

func foreignDeskOrder(coid string) bool {
	for _, p := range foreignDeskPrefixes {
		if strings.HasPrefix(coid, p) {
			return true
		}
	}
	return false
}

// Rehydrate re-adopts positions that survived a process restart. The exchange is the
// source of truth: every open position on the (dedicated) paper account is re-registered,
// its protective stop is re-attached (or freshly placed — a position must never sit
// unprotected), the allocator is re-funded with its cost so the shared budget can't be
// oversubscribed, and the Agent-3 manage loop resumes. Call synchronously at startup
// BEFORE any entry source (signal trader / dip hook) is wired. Returns adopted count.
func (m *Manager) Rehydrate(ctx context.Context) int {
	if m.broker == nil || !m.broker.Enabled() {
		return 0
	}
	positions, err := m.broker.Positions()
	if err != nil {
		log.Printf("[quant] rehydrate: positions fetch failed: %v", err)
		return 0
	}
	if len(positions) == 0 {
		return 0
	}
	orders, err := m.broker.allOrders()
	if err != nil {
		log.Printf("[quant] rehydrate: order history fetch failed: %v", err)
		orders = nil // still adopt with fresh stops; entry times fall back to now
	}

	adopted := 0
	for _, p := range positions {
		if p.Qty < 1 || p.AvgEntry <= 0 {
			continue // long-only whole-share desk; anything else isn't ours to manage
		}
		m.mu.Lock()
		_, exists := m.open[p.Symbol]
		m.mu.Unlock()
		if exists {
			continue
		}

		// Ownership guard: if the newest filled BUY of this symbol was placed by a sibling
		// desk (shared account), the position is theirs — leave it (and its stop) alone.
		// Positions with no order history at all are still adopted: on a dedicated account
		// they can only be ours (or the operator's, who wants them protected).
		var newestBuy *paperOrd
		for i := range orders {
			o := &orders[i]
			if o.Symbol != p.Symbol || o.Side != "buy" || o.Status != "filled" {
				continue
			}
			if newestBuy == nil || ordTime(*o).After(ordTime(*newestBuy)) {
				newestBuy = o
			}
		}
		if newestBuy != nil && foreignDeskOrder(newestBuy.ClientOrderID) {
			log.Printf("[quant] rehydrate: %s belongs to a sibling desk (%s) — not adopting", p.Symbol, newestBuy.ClientOrderID)
			continue
		}

		// Recover entry time (newest filled entry buy) and the newest working stop.
		entryTime := time.Now()
		var stop *paperOrd
		for i := range orders {
			o := &orders[i]
			if o.Symbol != p.Symbol || !strings.HasPrefix(o.ClientOrderID, QuantStrategy+"__") {
				continue
			}
			if o.Side == "buy" && o.Status == "filled" {
				entryTime = ordTime(*o)
			}
			if o.Side == "sell" && openOrderStatuses[o.Status] && (o.Type == "trailing_stop" || o.Type == "stop") {
				if stop == nil || ordTime(*o).After(ordTime(*stop)) {
					stop = o
				}
			}
		}

		stopID := ""
		stopPrice := round2(p.AvgEntry * (1 - m.trailPct/100)) // conservative floor guess
		if stop != nil {
			sq, _ := strconv.ParseFloat(stop.Qty, 64)
			if sq == p.Qty {
				stopID = stop.ID
				if sp, _ := strconv.ParseFloat(stop.StopPrice, 64); sp > 0 {
					stopPrice = sp
				}
			} else {
				// Wrong-size stop (process died mid-replace): replace it cleanly.
				_ = m.broker.Cancel(stop.ID)
			}
		}
		if stopID == "" {
			coid := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, p.Symbol, time.Now().UnixNano())
			id, serr := m.broker.TrailingStopSell(p.Symbol, p.Qty, m.trailPct, coid)
			if serr != nil {
				// Can't protect it → don't hold it (same invariant as at entry).
				log.Printf("[quant] rehydrate: %s stop placement failed (%v) — exiting to stay protected", p.Symbol, serr)
				ec := fmt.Sprintf("%s__%s__exit__No_Stop__%d", QuantStrategy, p.Symbol, time.Now().UnixNano())
				if _, e := m.broker.MarketSell(p.Symbol, p.Qty, ec); e != nil {
					log.Printf("[quant] CRITICAL: rehydrated %s held with NO stop and exit sell failed: %v", p.Symbol, e)
				}
				continue
			}
			stopID = id
		}

		pos := &managedPos{symbol: p.Symbol, qty: p.Qty, entryPrice: p.AvgEntry, entryTime: entryTime,
			stopOrderID: stopID, stopPrice: stopPrice, conf: 0.6, source: "rehydrated", strategy: "rehydrated"}
		m.mu.Lock()
		m.open[p.Symbol] = pos
		m.mu.Unlock()
		m.markLive(p.Symbol)
		if !m.alloc.Fund(p.Symbol, p.Qty*p.AvgEntry) {
			log.Printf("[quant] rehydrate: WARNING — allocator would not fund %s ($%.0f); budget accounting may under-count", p.Symbol, p.Qty*p.AvgEntry)
		}
		m.eng.logRec(LogRecord{Agent: "pipeline", Event: "order", Symbol: p.Symbol,
			Note: fmt.Sprintf("rehydrated after restart: %.0f @ $%.2f, stop re-attached", p.Qty, p.AvgEntry)})
		log.Printf("[quant] REHYDRATED %s %.0f @ $%.2f (stop %s) — manage loop resumed", p.Symbol, p.Qty, p.AvgEntry, stopID)
		go m.manage(ctx, pos)
		adopted++
	}
	return adopted
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

		// Deterministic time exit (rise-watch bounces): past the max hold, the move this
		// trade was built for is over — flatten regardless of what Agent 3 would say.
		if pos.maxHoldMin > 0 && time.Since(pos.entryTime) >= time.Duration(pos.maxHoldMin)*time.Minute {
			if m.forceExit(pos, "Time_Exit") {
				m.close(pos, fmt.Sprintf("max hold %dm reached", pos.maxHoldMin))
				return
			}
			continue
		}

		// D — breakeven ratchet (mechanical, runs for the position's whole life).
		m.breakevenRatchet(pos)

		// E — grace checkpoints (mechanical): one strict look at grace/2, one normal look
		// at grace end. Exits only a position that is BOTH meaningfully red (in units of
		// its own planned risk) AND below session VWAP.
		if m.graceCheckpoints(pos) {
			return
		}

		// No Agent 3? The trailing stop manages it on its own.
		if m.agent3 == nil || !m.agent3.Enabled() {
			continue
		}

		// Mechanical grace period: within the first exitGraceMin minutes the LLM is not
		// consulted — the deterministic protections above (exchange stop, max hold, EOD,
		// ratchet, checkpoints) are the only exits. A short wiggle below entry is noise,
		// not a broken thesis.
		if exitGraceMin > 0 && time.Since(pos.entryTime) < time.Duration(exitGraceMin)*time.Minute {
			continue
		}

		snap := m.eng.exitSnapshot(sym, pos, pos.entryPrice, pos.qty, pos.stopPrice, pos.entryTime)
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

// riskDist returns the position's planned risk per share: entry − original stop when the
// entry set one (rise = the dip low, signal = the bracket stop), else the trailing-stop
// distance (dip trades). This is the "R" every mechanical exit rail is measured in.
func (m *Manager) riskDist(pos *managedPos) float64 {
	if pos.origStop > 0 && pos.origStop < pos.entryPrice {
		return pos.entryPrice - pos.origStop
	}
	return pos.entryPrice * m.trailFor(pos) / 100
}

// breakevenRatchet (rail D): once price has reached entry + beRatchetR×R, replace the
// protective stop with a fixed stop AT entry. Ratchet-up only, once per position; uses
// the same confirm-cancel-then-place path as Agent 3's tighten_stop so there can never
// be two stops (oversell) or zero stops (unprotected).
func (m *Manager) breakevenRatchet(pos *managedPos) {
	if beRatchetR <= 0 || pos.beDone {
		return
	}
	if pos.stopPrice >= pos.entryPrice {
		pos.beDone = true // already at/above breakeven (e.g. Agent 3 tightened past it)
		return
	}
	cur := m.eng.LastClose(pos.symbol)
	r := m.riskDist(pos)
	if cur <= 0 || r <= 0 || cur < pos.entryPrice+beRatchetR*r {
		return
	}
	// On a TRAILING stop (stopPrice 0): if the trail's floor has already ratcheted to or
	// past breakeven (conservative bound: current price × (1 − trail%)), replacing it
	// with a fixed stop AT entry would LOOSEN protection and freeze the upward ratchet —
	// keep the trail instead. (Real scenario: price gaps +2% between ticks; trail floor
	// is now entry+0.5% while a breakeven stop would sit below it at entry.)
	if pos.stopPrice == 0 && cur*(1-m.trailFor(pos)/100) >= pos.entryPrice {
		pos.beDone = true
		return
	}
	be := pos.entryPrice
	if be >= cur {
		return // market slipped back under entry between checks — retry next pass
	}
	if !m.cancelAndConfirm(pos.stopOrderID) {
		return // old stop filled/unconfirmed — next pass reconciles (flat → close path)
	}
	coid := fmt.Sprintf("%s__%s__exit__BE_Stop__%d", QuantStrategy, pos.symbol, time.Now().UnixNano())
	id, err := m.broker.StopSell(pos.symbol, pos.qty, be, coid)
	if err != nil {
		// Never left unprotected: fall back to a fresh trailing stop — and STOP trying.
		// The trail is full pre-ratchet protection; retrying every tick would cancel a
		// good stop and re-place it in a loop (a per-position API storm, the 2026-07-16
		// failure shape) for a nice-to-have upgrade. One attempt, then stand down.
		tc := fmt.Sprintf("%s__%s__exit__Trail_Stop__%d", QuantStrategy, pos.symbol, time.Now().UnixNano())
		if pid, perr := m.broker.TrailingStopSell(pos.symbol, pos.qty, m.trailFor(pos), tc); perr == nil {
			pos.stopOrderID = pid
			pos.stopPrice = 0
			pos.beDone = true
			m.eng.logRec(LogRecord{Agent: "pipeline", Event: "order", Symbol: pos.symbol,
				Note: "breakeven ratchet: stop placement failed — kept trailing stop, not retrying"})
		}
		return
	}
	pos.stopOrderID = id
	pos.stopPrice = be
	pos.beDone = true
	m.eng.logRec(LogRecord{Agent: "pipeline", Event: "order", Symbol: pos.symbol,
		Note: fmt.Sprintf("breakeven ratchet: +%.2fR reached — stop moved to entry $%.2f (worst case now $0)", beRatchetR, be)})
}

// graceCheckpoints (rail E): two one-shot mechanical inspections — at grace/2 with the
// strict threshold (chkHalfR×R) and at grace end with the normal one (chkFullR×R). A
// position must be BOTH that far underwater AND below session VWAP to be cut; either
// condition alone is survivable noise. Returns true when the position was closed.
func (m *Manager) graceCheckpoints(pos *managedPos) bool {
	if exitGraceMin <= 0 {
		return false
	}
	held := time.Since(pos.entryTime)
	full := time.Duration(exitGraceMin) * time.Minute
	half := full / 2
	check := func(fracR float64, label string) bool {
		if fracR <= 0 {
			return false
		}
		cur := m.eng.LastClose(pos.symbol)
		r := m.riskDist(pos)
		if cur <= 0 || r <= 0 {
			return false
		}
		loss := pos.entryPrice - cur
		if loss < fracR*r {
			return false
		}
		_, _, vwap := m.eng.sessionAgg(pos.symbol)
		if vwap <= 0 || cur >= vwap {
			return false
		}
		if !m.forceExit(pos, "Checkpoint_Exit") {
			return false // stop unconfirmed — deterministic protections still standing; retry next pass
		}
		m.eng.logRec(LogRecord{Agent: "pipeline", Event: "order", Symbol: pos.symbol,
			Note: fmt.Sprintf("%s checkpoint: down %.2fR and below VWAP — mechanical exit", label, loss/r)})
		m.close(pos, label+" checkpoint exit")
		return true
	}
	if !pos.chkHalf && held >= half && held < full {
		pos.chkHalf = true
		return check(chkHalfR, "mid-grace")
	}
	if !pos.chkFull && held >= full {
		pos.chkFull = true
		return check(chkFullR, "grace-end")
	}
	return false
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
			if pid, perr := m.broker.TrailingStopSell(sym, pos.qty, m.trailFor(pos), tc); perr == nil {
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
		// Noise floor: an exit_now on a LOSING position must be backed by a real move —
		// at least exitNoiseR of the trade's own planned risk. Profit-side exits always
		// pass. (2026-07-16 audit: 7 of 9 LLM exits fired at ≤0.15% red — tick noise
		// narrated as "structure broken".)
		if exitNoiseR > 0 && cur > 0 && cur < pos.entryPrice {
			if r := m.riskDist(pos); r > 0 && pos.entryPrice-cur < exitNoiseR*r {
				m.eng.logRec(LogRecord{Agent: "agent3_exit", Event: "skip", Symbol: sym,
					Note: fmt.Sprintf("exit_now vetoed by noise floor: down %.2fR < %.2fR — deterministic plan continues (%s)",
						(pos.entryPrice-cur)/r, exitNoiseR, d.Reason)})
				return false
			}
		}
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
		if pid, perr := m.broker.TrailingStopSell(pos.symbol, qty, m.trailFor(pos), tc); perr == nil {
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
	m.alloc.Release(sym)
	pnl := 0.0
	if px := m.eng.LastClose(sym); px > 0 {
		pnl = (px - pos.entryPrice) * pos.qty
	}
	// Source-tagged outcome so the report can attribute realized P&L per pipeline (dip vs
	// signal) and build the dip-agent scorecard. P&L is the manager's approximate
	// mark-to-close; the broker reconstruction remains the accounting source of truth.
	m.eng.logRec(LogRecord{Agent: "pipeline", Event: "outcome", Symbol: sym, Note: note,
		Output: map[string]interface{}{
			"source": pos.source, "pnl": round2(pnl), "win": pnl > 0,
			"conf": pos.conf, "held_min": round2(time.Since(pos.entryTime).Minutes()),
		}})
	log.Printf("[quant] CLOSED %s (%s) — %s; approx P&L $%.2f; capital released", sym, pos.source, note, pnl)
	if m.OnClosed != nil {
		m.OnClosed(sym, pnl)
	}
}

func (m *Manager) log(agent, event, sym, note string) {
	m.eng.logRec(LogRecord{Agent: agent, Event: event, Symbol: sym, Note: note})
}
