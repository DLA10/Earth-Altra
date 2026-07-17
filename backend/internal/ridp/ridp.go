// Package ridp is the two-strategy paper desk ("RIDP" = RIDER + DIPPER) distilled from
// the 2026-07 research week: the ONLY two behaviors that survived 12-month/5-year
// falsification testing, both originally the operator's own patterns.
//
//   RIDER  — intraday momentum: buy the day's leader (+1% from open, above rising VWAP,
//            2x volume, QQQ green, 10:30-14:30 ET), trail 3.5% from the intrabar peak,
//            tighten to 2% once up +3%, flat by 15:55. Validated OOS ≈ +$2.2/trade at a
//            $1,500 slice, ~13 trades/wk.
//   DIPPER — multi-day dip reversal: after 3+ red closes (or −6% in 5 sessions), buy the
//            next open once a day CLOSES above the prior day's high; hard GTC stop at
//            entry − 2×ATR(14); exit when price falls 2.5×ATR below the highest close;
//            max hold 40 sessions. Validated over 5 years ≈ +0.35R OOS.
//
// Design rules (all deliberate): 100% deterministic — NO LLM anywhere on the trade path;
// budget management is pure code (allocator below); trades on its OWN paper account
// (PAPER_RIDP_* keys — strict one account per desk since 2026-07-16) via the
// quant.Broker wrapper with "ridp_" client-order-id prefixes so P&L attribution never
// mixes with the AI quant desks (which run untouched, side by side).
// Every decision journals to data/ridp/<day>.jsonl; open state persists to
// data/ridp/state.json and is rehydrated (and broker-verified) after a restart.
package ridp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

// Fixed tunables — exit/risk rule cards, unchanged since falsification testing. Exits and
// risk sizing are NOT loosened in throughput mode; only entry gates are (var block below).
const (
	riderTrailPct  = 0.035 // initial trail from the intrabar peak
	riderTightPct  = 0.020 // tightened trail ...
	riderTightTrig = 0.03  // ... once the peak is +3% above entry
	riderSlice     = 1500  // $ per RIDER position
	riderLastMin   = 300   // no fresh entries after 14:30 ET
	riderFlatHour  = 15    // flatten from 15:55 ET
	riderFlatMin   = 55

	dipperStopATR  = 2.0 // hard GTC stop distance (this is 1R)
	dipperTrailATR = 2.5 // exit when price <= highest close - 2.5*ATR
	dipperRiskUSD  = 50.0
	dipperMaxNotnl = 2000.0
	dipperSlots    = 3
	dipperMaxHold  = 40 // sessions

	scanEvery = 10 * time.Second

	sessionMinutes = 390 // regular session length (09:30–16:00 ET); RVOL time-of-day baseline
)

// Throughput-mode ENTRY dials (2026-07-16). The originals were paid for with 12-month/
// 5-year falsification tests; they produced ~0 trades/2wk live, so entries are loosened
// for the paper measurement month. Originals + rationale: THROUGHPUT_MODE.md. Each is
// overridable via env — set the env var to the original value to roll back per-dial
// without a code change.
var (
	riderGainMin  = envDial("RIDP_RIDER_GAIN_MIN", 0.007)   // original 0.01  (+1% from open)
	riderRVOLMin  = envDial("RIDP_RIDER_RVOL_MIN", 1.5)     // original 2.0   (2x time-of-day volume)
	riderQQQMin   = envDial("RIDP_RIDER_QQQ_MIN", -0.0015)  // original 0.0   (QQQ strictly green)
	riderStartMin = envDialInt("RIDP_RIDER_START_MIN", 30)  // original 60    (entries from 10:30 ET)
	riderSlots    = envDialInt("RIDP_RIDER_SLOTS", 3)       // original 2
	dipperRedDays = envDialInt("RIDP_DIPPER_RED_DAYS", 2)   // original 3     (consecutive red closes)
	dipperDrop5d  = envDial("RIDP_DIPPER_DROP_5D", -0.04)   // original -0.06 (5-session drop)
	dipperTurnPct = envDial("RIDP_DIPPER_TURN_PCT", 0.015)  // original: none (alt turn trigger: close > prev close +1.5% on above-avg volume; 0 disables)
	reverterTopN  = envDialInt("RIDP_REVERTER_TOP_N", 55)   // original: top 1/3 of universe by ATR% (~53 of 160); fixed count so the 534-name universe doesn't 3x REVERTER's pond
)

func envDial(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		log.Printf("ridp: ignoring invalid %s=%q (using %v)", key, v, def)
	}
	return def
}

func envDialInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("ridp: ignoring invalid %s=%q (using %v)", key, v, def)
	}
	return def
}

// Position is one open RIDP position (either strategy).
type Position struct {
	Strategy  string    `json:"strategy"` // "rider" | "dipper"
	Symbol    string    `json:"symbol"`
	Qty       float64   `json:"qty"`
	Entry     float64   `json:"entry"`
	OpenedAt  time.Time `json:"opened_at"`
	Peak      float64   `json:"peak"`       // rider: intrabar high peak; dipper: highest CLOSE
	StopID    string    `json:"stop_id"`    // exchange-side protective order id
	HardStop  float64   `json:"hard_stop"`  // dipper: entry - 2*ATR; rider: informational
	ATR       float64   `json:"atr"`        // dipper: daily ATR(14) at entry
	Tightened bool      `json:"tightened"`  // rider: 2% trail engaged
	Sessions  int       `json:"sessions"`   // dipper: sessions held
	LastDay   string    `json:"last_day"`   // dipper: last session counted

	// Closing state (2026-07-17 leak fix): an exit is IN FLIGHT. The position stays on
	// the books until the account confirms flat — never finalize a fire-and-forget sell.
	ExitID     string `json:"exit_id,omitempty"`
	ExitReason string `json:"exit_reason,omitempty"`
}

// Trade is one closed round trip.
type Trade struct {
	Strategy string    `json:"strategy"`
	Symbol   string    `json:"symbol"`
	Qty      float64   `json:"qty"`
	Entry    float64   `json:"entry"`
	Exit     float64   `json:"exit"`
	PnL      float64   `json:"pnl"`
	Reason   string    `json:"reason"`
	OpenedAt time.Time `json:"opened_at"`
	ClosedAt time.Time `json:"closed_at"`
}

// dailyCtx is the per-symbol daily context DIPPER and RIDER share.
type dailyCtx struct {
	ATR       float64   // ATR(14)
	AvgVol    float64   // 14-day average daily volume (RIDER RVOL baseline)
	PrevHigh  float64   // yesterday's high (DIPPER trigger reference is set in setups)
	Setup     bool      // DIPPER: symbol is in a qualified falling setup
	Triggered bool      // DIPPER: yesterday closed above the prior day's high
	AsOf      string    // ET date the stats were computed for
}

// Manager runs both strategies.
type Manager struct {
	broker  *quant.Broker
	engine  *candles.Engine
	symbols []string
	etz     *time.Location
	dataDir string
	live    bool // false = journal-only shadow

	ensureLive func(string) // subscribe trades/quotes so the UI ticks (nil-safe)
	dailyFn    func([]string, int) (map[string][]DailyBar, error)

	mu       sync.Mutex
	volProf  map[string][]float64 // per-symbol intraday cumulative-volume curve (len sessionMinutes)
	open     map[string]*Position // keyed symbol (one position per symbol across strategies)
	closed   []Trade
	daily    map[string]*dailyCtx
	dailyDay string          // ET date the daily context was refreshed for
	entered  map[string]bool // rider: symbol entered today (one shot/day)
	dayKey   string
	lastSkip map[string]time.Time // journal throttle for repeating allocator skips

	// Scale plumbing (2026-07-16 incident: with ~50 REVERTER positions on a $100k
	// account, per-position API calls every tick blew Alpaca's rate limit; the 429
	// storms then corrupted entry confirmation and ghost shares accumulated to −$164k
	// cash). ONE batched /positions call per tick + a cached account snapshot keep the
	// desk's API usage flat no matter how many positions the budget allows.
	livePos    map[string]float64 // symbol -> qty from the last batched positions fetch
	livePosAt  time.Time          // when the snapshot was taken (zero = never)
	acctInfo   quant.AccountInfo  // cached account snapshot for the allocator
	acctAt     time.Time

	// Ghost reconciliation (2026-07-17 incident: REVERTER's rapid same-symbol re-entry
	// crossed in-flight exit orders and leaked shares the desk didn't track — 16
	// unprotected positions held overnight; the old alarm missed them because the
	// symbols were PARTIALLY tracked, and the flatten's shared-account guard sold only
	// the tracked qty). This account is DEDICATED (strict one desk per account), so
	// every share on it is ours: untracked/excess shares get market-flattened during
	// market hours, throttled per symbol.
	lastExit     map[string]time.Time // symbol -> last exit time (re-entry cooldown)
	lastGhostFix map[string]time.Time // symbol -> last reconcile attempt (throttle)
	booksBad     bool                 // state.json corrupt: reconcile stands down (alarm only)
}

// DailyBar is the minimal daily bar the manager needs (decoupled from the alpaca pkg).
type DailyBar struct {
	Day    string // ET date
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// VolBar is a minimal 1-minute bar (open time + volume) used to build the intraday volume
// profile that makes RIDER's RVOL gate time-of-day-aware.
type VolBar struct {
	Time   int64 // unix seconds (bar open)
	Volume float64
}

// SetVolumeProfiles installs per-symbol intraday cumulative-volume curves (built from a
// historical intraday fetch in main.go). Safe to call at any time; symbols without a profile
// simply fall back to the flat linear RVOL estimate until one lands.
func (m *Manager) SetVolumeProfiles(profiles map[string][]float64) {
	m.mu.Lock()
	n := 0
	for sym, p := range profiles {
		if len(p) == sessionMinutes {
			m.volProf[sym] = p
			n++
		}
	}
	m.mu.Unlock()
	log.Printf("ridp: installed intraday volume profiles for %d symbols (RVOL now time-of-day-aware)", n)
}

// expectedVolFrac returns the fraction of a normal day's volume expected to have traded by
// sessionMin (minute of the 09:30 session). With a learned intraday curve installed it
// reflects the real U-shape (heavy at the open/close, light midday); without one it falls
// back to the flat linear estimate the gate used before profiles existed. This is what makes
// "2x normal FOR THIS TIME OF DAY" honest instead of assuming volume accrues evenly.
func (m *Manager) expectedVolFrac(sym string, sessionMin int) float64 {
	m.mu.Lock()
	prof := m.volProf[sym]
	m.mu.Unlock()
	if len(prof) == sessionMinutes {
		i := sessionMin
		if i < 0 {
			i = 0
		}
		if i >= sessionMinutes {
			i = sessionMinutes - 1
		}
		if prof[i] > 0 {
			return prof[i]
		}
	}
	frac := float64(sessionMin) / float64(sessionMinutes)
	if frac < 0.05 {
		frac = 0.05
	}
	if frac > 1 {
		frac = 1
	}
	return frac
}

// BuildVolumeProfile turns several sessions of regular-hours 1-minute bars into a cumulative
// volume curve cumFrac[sessionMinutes]: cumFrac[m] is the average fraction of a day's volume
// traded by minute m of the 09:30–16:00 ET session. It mirrors scanner.BuildVolumeProfile so
// both desks measure "normal for this time of day" the same way. Returns nil if no usable
// session is present (caller then keeps the linear fallback).
func BuildVolumeProfile(etz *time.Location, bars []VolBar) []float64 {
	if etz == nil {
		etz = time.UTC
	}
	// Group regular-session bars by ET calendar day.
	byDay := map[string][]VolBar{}
	for _, b := range bars {
		t := time.Unix(b.Time, 0).In(etz)
		if !regularSessionET(t) {
			continue
		}
		byDay[t.Format("2006-01-02")] = append(byDay[t.Format("2006-01-02")], b)
	}
	sum := make([]float64, sessionMinutes)
	cnt := make([]int, sessionMinutes)
	for _, dayBars := range byDay {
		perMin := make([]float64, sessionMinutes)
		used := false
		for _, b := range dayBars {
			t := time.Unix(b.Time, 0).In(etz)
			anchor := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, etz).Unix()
			if idx := int((b.Time - anchor) / 60); idx >= 0 && idx < sessionMinutes {
				perMin[idx] += b.Volume
				used = true
			}
		}
		if !used {
			continue
		}
		cum := make([]float64, sessionMinutes)
		run := 0.0
		for i := 0; i < sessionMinutes; i++ {
			run += perMin[i]
			cum[i] = run
		}
		total := cum[sessionMinutes-1]
		if total <= 0 {
			continue
		}
		for i := 0; i < sessionMinutes; i++ {
			sum[i] += cum[i] / total
			cnt[i]++
		}
	}
	out := make([]float64, sessionMinutes)
	nonEmpty := false
	var last float64
	for i := 0; i < sessionMinutes; i++ {
		if cnt[i] > 0 {
			out[i] = sum[i] / float64(cnt[i])
			nonEmpty = true
		} else {
			out[i] = last // carry forward across gaps
		}
		if out[i] < last {
			out[i] = last // enforce monotonic non-decreasing
		}
		last = out[i]
	}
	if !nonEmpty {
		return nil
	}
	return out
}

// regularSessionET reports whether an ET timestamp is a weekday inside 09:30–16:00.
func regularSessionET(t time.Time) bool {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	mins := t.Hour()*60 + t.Minute()
	return mins >= 9*60+30 && mins < 16*60
}

func New(broker *quant.Broker, engine *candles.Engine, symbols []string, etz *time.Location,
	dataDir string, live bool, dailyFn func([]string, int) (map[string][]DailyBar, error)) *Manager {
	if etz == nil {
		etz = time.UTC
	}
	m := &Manager{
		broker: broker, engine: engine, symbols: symbols, etz: etz,
		dataDir: filepath.Join(dataDir, "ridp"), live: live, dailyFn: dailyFn,
		open: map[string]*Position{}, daily: map[string]*dailyCtx{}, entered: map[string]bool{},
		volProf: map[string][]float64{}, lastSkip: map[string]time.Time{},
		lastExit: map[string]time.Time{}, lastGhostFix: map[string]time.Time{},
	}
	_ = os.MkdirAll(m.dataDir, 0o755)
	m.loadState()
	return m
}

// SetEnsureLive wires the on-demand streaming activation (so the RIDP page's quotes tick).
func (m *Manager) SetEnsureLive(fn func(string)) { m.ensureLive = fn }

func (m *Manager) Enabled() bool { return m != nil && m.broker.Enabled() }

// Start launches the scan loop and the daily-context refresher.
func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		log.Printf("ridp: disabled (no paper broker keys)")
		return
	}
	m.refreshDaily()
	m.rehydrate()
	go func() {
		t := time.NewTicker(scanEvery)
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
	mode := "LIVE (paper)"
	if !m.live {
		mode = "SHADOW"
	}
	log.Printf("ridp: %s | RIDER $%d x%d slots (1%%/2xRVOL entry, 3.5%%->2%% trail, 15:55 flat) | DIPPER $%.0f-risk x%d slots (2xATR stop, 2.5xATR trail) | REVERTER $%d slice, budget-capped/no trade cap (%dm mean, %.1fsigma entry, exit at mean, high-amp only) | %d symbols",
		mode, riderSlice, riderSlots, dipperRiskUSD, dipperSlots, reverterSlice, reverterWindow, -reverterZIn, len(m.symbols))
}

// tick is the 10-second heartbeat: day rollover, daily refresh, entries, exits.
func (m *Manager) tick() {
	now := time.Now().In(m.etz)
	day := now.Format("2006-01-02")

	m.mu.Lock()
	if day != m.dayKey {
		m.dayKey = day
		m.entered = map[string]bool{}
	}
	needDaily := m.dailyDay != day && now.Hour()*60+now.Minute() >= 9*60+20
	m.mu.Unlock()
	if needDaily {
		m.refreshDaily()
	}

	mins := now.Hour()*60 + now.Minute()
	offHours := now.Weekday() == time.Saturday || now.Weekday() == time.Sunday ||
		mins < 9*60+30 || mins >= 16*60
	if offHours {
		// Keep the broker-truth snapshot fresh at a slow cadence even off-hours, so the
		// report's ghost view stays honest around the clock (reconcile itself only ever
		// acts during the session, below).
		m.mu.Lock()
		stale := m.livePosAt.IsZero() || time.Since(m.livePosAt) > time.Minute
		m.mu.Unlock()
		if stale {
			m.refreshLivePositions()
		}
		return
	}
	sessionMin := mins - (9*60 + 30)

	m.refreshLivePositions()
	m.reconcileGhosts()
	m.manageRider(now, sessionMin)
	m.manageDipper(now, sessionMin)
	m.manageReverter(now)
	if sessionMin >= riderStartMin && sessionMin < riderLastMin {
		m.scanRiderEntries(now, sessionMin)
	}
	if sessionMin >= 1 && sessionMin <= 20 { // 09:31-09:50 ET window for dipper entries
		m.scanDipperEntries(now)
	}
	if sessionMin >= reverterStartMin && sessionMin < reverterLastMin {
		m.scanReverterEntries(now)
	}
}

// ---- batched exchange snapshots (flat API usage regardless of position count) ----

// refreshLivePositions pulls the account's positions in ONE call and caches the
// symbol→qty map for this tick. All per-position "still held?" checks read this map.
func (m *Manager) refreshLivePositions() {
	positions, err := m.broker.Positions()
	if err != nil {
		return // keep the previous snapshot; readers check freshness
	}
	snap := make(map[string]float64, len(positions))
	for _, p := range positions {
		snap[p.Symbol] = p.Qty
	}
	m.mu.Lock()
	// Visibility: untracked shares on this dedicated account are ghosts (a crashed
	// entry path) or manual trades — say so loudly instead of silently ignoring them.
	for sym, qty := range snap {
		if _, tracked := m.open[sym]; !tracked && qty > 0 {
			if time.Since(m.lastSkip["ghost|"+sym]) > 10*time.Minute {
				m.lastSkip["ghost|"+sym] = time.Now()
				log.Printf("ridp: ⚠ account holds %.0f %s NOT tracked by any strategy — ghost or manual position, please review", qty, sym)
			}
		}
	}
	m.livePos = snap
	m.livePosAt = time.Now()
	m.mu.Unlock()
}

// reconcileGhosts flattens shares on this DEDICATED account that no strategy tracks:
// fully untracked symbols, and the excess when the account holds more than the tracked
// position (both are entry-tracking leaks — e.g. a buy crossing an in-flight exit).
// Runs only during market hours (tick gates that), throttled per symbol, reads the
// batched snapshot (no extra polling). This is the fix for 2026-07-17: 16 leaked
// positions sat overnight with no stops and no owner.
func (m *Manager) reconcileGhosts() {
	m.mu.Lock()
	if m.booksBad {
		m.mu.Unlock()
		return // books unreliable (corrupt state.json): never sell on a guess
	}
	if m.livePosAt.IsZero() || time.Since(m.livePosAt) > 45*time.Second {
		m.mu.Unlock()
		return // no fresh truth — don't act on guesses
	}
	type fix struct {
		sym     string
		sell    float64
		tracked float64
	}
	var fixes []fix
	for sym, qty := range m.livePos {
		if qty <= 0 {
			continue
		}
		pos, tracked := m.open[sym]
		excess := qty
		trackedQty := 0.0
		if tracked {
			trackedQty = pos.Qty
			excess = qty - pos.Qty
		}
		if excess < 1 { // whole-share desk; fractions can't be ours
			continue
		}
		// NEVER act while one of our own exits might still be in flight for this symbol
		// (double-selling an in-flight exit would short the account). The order resolves
		// in seconds; the ghost can wait two minutes.
		if time.Since(m.lastExit[sym]) < 2*time.Minute {
			continue
		}
		if time.Since(m.lastGhostFix[sym]) < 3*time.Minute {
			continue
		}
		m.lastGhostFix[sym] = time.Now()
		fixes = append(fixes, fix{sym: sym, sell: excess, tracked: trackedQty})
	}
	m.mu.Unlock()
	for _, f := range fixes {
		coid := fmt.Sprintf("ridp_ghost__%s__flatten__%d", f.sym, time.Now().UnixNano())
		if _, err := m.broker.MarketSell(f.sym, f.sell, coid); err != nil {
			m.journal("ghost", "error", f.sym,
				fmt.Sprintf("ghost flatten failed (%.0f untracked, %.0f tracked): %v", f.sell, f.tracked, err))
			continue
		}
		m.journal("ghost", "exit", f.sym,
			fmt.Sprintf("flattened %.0f UNTRACKED share(s) (%.0f tracked) — leaked by a crossed entry/exit, not any strategy's position", f.sell, f.tracked))
		log.Printf("ridp: ⚠ ghost reconcile: sold %.0f untracked %s", f.sell, f.sym)
	}
}

// liveQty reads a symbol's held quantity from the batched snapshot. ok=false when the
// snapshot is missing or stale (>45s) — callers must then DEFER close/finalize decisions.
func (m *Manager) liveQty(sym string) (qty float64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.livePosAt.IsZero() || time.Since(m.livePosAt) > 45*time.Second {
		return 0, false
	}
	return m.livePos[sym], true
}

// account returns a cached account snapshot (10s TTL; rides through brief API blips on
// the last good value for up to a minute). The allocator calls this on every candidate,
// which used to be a fresh HTTP call each time.
func (m *Manager) account() (quant.AccountInfo, error) {
	m.mu.Lock()
	cached, at := m.acctInfo, m.acctAt
	m.mu.Unlock()
	if !at.IsZero() && time.Since(at) < 10*time.Second {
		return cached, nil
	}
	ai, err := m.broker.Account()
	if err != nil {
		if !at.IsZero() && time.Since(at) < time.Minute {
			return cached, nil
		}
		return quant.AccountInfo{}, err
	}
	m.mu.Lock()
	m.acctInfo, m.acctAt = ai, time.Now()
	m.mu.Unlock()
	return ai, nil
}

// ---- allocator (pure code, as requested: no AI) ----

// alloc reports whether a new position of `strategy` with `notional` can be funded:
// per-strategy slot caps AND total deployed + new notional must fit the account's
// buying power with a 20% headroom (fetched live from the paper account each call —
// the "budget allocator that fetches the buying power").
func (m *Manager) alloc(strategy string, notional float64) (bool, string) {
	m.mu.Lock()
	slots, deployed := 0, 0.0
	for _, p := range m.open {
		deployed += p.Qty * p.Entry
		if p.Strategy == strategy {
			slots++
		}
	}
	m.mu.Unlock()
	maxSlots := riderSlots
	switch strategy {
	case "dipper":
		maxSlots = dipperSlots
	case "reverter":
		maxSlots = 1 << 30 // no per-strategy trade cap (operator's request); only the budget limits it
	}
	if slots >= maxSlots {
		return false, fmt.Sprintf("no free %s slot (%d/%d)", strategy, slots, maxSlots)
	}
	ai, err := m.account()
	if err != nil {
		return false, "account fetch failed: " + err.Error()
	}
	budget := ai.BuyingPower * 0.8
	if ai.Equity > 0 && ai.Equity < budget {
		budget = ai.Equity * 0.8
	}
	if deployed+notional > budget {
		return false, fmt.Sprintf("budget: deployed $%.0f + new $%.0f > $%.0f (80%% of account)", deployed, notional, budget)
	}
	return true, ""
}

// ---- shared open/close plumbing ----

func (m *Manager) openPosition(strategy, sym string, qty float64, atr, hardStop float64, now time.Time) {
	if !m.live {
		m.journal(strategy, "shadow", sym, fmt.Sprintf("SHADOW: would buy %.0f shares", qty))
		return
	}
	coid := fmt.Sprintf("ridp_%s__%s__entry__%d", strategy, sym, time.Now().UnixNano())
	id, err := m.broker.MarketBuy(sym, qty, coid)
	if err != nil {
		m.journal(strategy, "error", sym, "entry failed: "+err.Error())
		return
	}
	// The order was ACCEPTED — from here on we NEVER walk away without tracking the
	// position. Abandoning an unconfirmed-but-filled market buy is how untracked ghost
	// shares accumulated during the 2026-07-16 rate-limit storm (−$164k cash).
	fq, ap := m.awaitFill(id, 12*time.Second)
	if fq <= 0 {
		q2, qerr := m.broker.PositionQty(sym)
		switch {
		case qerr == nil && q2 > 0:
			fq = q2
		case qerr == nil:
			// Confirmed: nothing filled and nothing held. Safe to stop here.
			m.journal(strategy, "error", sym, "entry not confirmed filled (position confirmed empty)")
			return
		default:
			// Can't confirm either way (e.g. rate-limited). An accepted market order on a
			// liquid name essentially always fills — assume the requested qty and track
			// it; the batched snapshot reconciles the truth next tick.
			fq = qty
			m.journal(strategy, "error", sym, "entry fill unconfirmed ("+qerr.Error()+") — tracking requested qty, will reconcile")
		}
	}
	if fq > 0 && fq < qty {
		// Partial-fill SNAPSHOT, not a partial order: an accepted market DAY order on a
		// liquid name completes — tracking only the snapshot is how shares leak. Track
		// the requested quantity; exits sell the account's real quantity regardless.
		m.journal(strategy, "entry", sym, fmt.Sprintf("fill snapshot %.0f/%.0f at confirm timeout — tracking full requested qty", fq, qty))
		fq = qty
	}
	if ap <= 0 {
		ap = m.lastPrice(sym)
	}
	// Exchange-side protection. If placement fails, we still REGISTER the position
	// (StopID empty) and the manage loop retries protection every tick — software exits
	// run regardless, so it is never unmanaged and never an untracked ghost.
	var stopID string
	var serr error
	if strategy == "rider" {
		sc := fmt.Sprintf("ridp_rider__%s__trail__%d", sym, time.Now().UnixNano())
		stopID, serr = m.broker.TrailingStopSell(sym, fq, riderTrailPct*100, sc)
	} else {
		sc := fmt.Sprintf("ridp_%s__%s__stop__%d", strategy, sym, time.Now().UnixNano())
		stopID, serr = m.broker.StopSell(sym, fq, hardStop, sc)
	}
	if serr != nil {
		stopID = ""
		m.journal(strategy, "error", sym, "protective order failed ("+serr.Error()+") — tracked UNPROTECTED, retrying protection each tick")
		log.Printf("ridp: %s %s protective order failed (%v) — tracked unprotected, retrying", strategy, sym, serr)
	}
	pos := &Position{Strategy: strategy, Symbol: sym, Qty: fq, Entry: ap, OpenedAt: now,
		Peak: ap, StopID: stopID, HardStop: hardStop, ATR: atr, LastDay: now.Format("2006-01-02")}
	m.mu.Lock()
	m.open[sym] = pos
	m.mu.Unlock()
	m.saveState()
	if m.ensureLive != nil {
		m.ensureLive(sym)
	}
	m.journal(strategy, "entry", sym, fmt.Sprintf("bought %.0f @ $%.2f (protection %s)", fq, ap, stopID))
	log.Printf("[ridp] %s ENTER %s %.0f @ $%.2f", strategy, sym, fq, ap)
}

// resolveClosing advances a position whose exit order is in flight. Returns true while
// the position is busy closing (callers skip all other management). The books release
// the position ONLY when the account confirms flat — the fire-and-forget finalize that
// leaked untracked shares on 2026-07-17 is gone.
func (m *Manager) resolveClosing(p *Position) bool {
	if p.ExitID == "" {
		return false
	}
	qty, ok := m.liveQty(p.Symbol)
	if !ok {
		return true // no fresh account truth — keep waiting, do nothing else
	}
	if qty <= 0 {
		// Confirmed flat: record the exit at its real fill price when available.
		px := 0.0
		if _, ap, st, err := m.broker.Order(p.ExitID); err == nil && st == "filled" && ap > 0 {
			px = ap
		}
		m.finalize(p, px, p.ExitReason)
		return true
	}
	// Still holding shares — what happened to the exit order?
	_, _, st, err := m.broker.Order(p.ExitID)
	if err != nil {
		return true // transient; check again next tick
	}
	switch st {
	case "canceled", "expired", "rejected", "done_for_day":
		// The sell died with shares still held: back to normal management, which will
		// retry the exit (and protection) on the next pass.
		m.journal(p.Strategy, "error", p.Symbol,
			fmt.Sprintf("exit order %s with %.0f share(s) still held — resuming management, will retry", st, qty))
		p.ExitID, p.ExitReason = "", ""
		m.saveState()
		return false
	case "filled":
		// Filled but shares remain (partial books mismatch): keep the position at the
		// account's real quantity and let management exit the remainder.
		m.journal(p.Strategy, "error", p.Symbol,
			fmt.Sprintf("exit filled but %.0f share(s) remain — tracking remainder, will re-exit", qty))
		p.Qty = qty
		p.ExitID, p.ExitReason = "", ""
		m.saveState()
		return false
	default:
		return true // new / accepted / partially_filled — still working
	}
}

// closePosition market-exits a position (confirm-cancel the protective order first).
func (m *Manager) closePosition(pos *Position, reason string) {
	if pos.ExitID != "" {
		m.resolveClosing(pos) // an exit is already in flight — never send a second sell
		return
	}
	if pos.StopID != "" {
		_ = m.broker.Cancel(pos.StopID)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, st, err := m.broker.Order(pos.StopID); err == nil {
				if st == "canceled" || st == "expired" || st == "rejected" || st == "replaced" {
					break
				}
				if st == "filled" { // the stop beat us to it
					m.finalize(pos, 0, "protective stop filled")
					return
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
	}
	qty, err := m.broker.PositionQty(pos.Symbol)
	if err != nil {
		// Transient broker error: do NOT finalize (that would strand real shares as
		// untracked "ghosts"). Keep the position; the manage loop retries next tick and
		// the exchange-side protection still stands.
		m.journal(pos.Strategy, "error", pos.Symbol, "exit deferred: position check failed: "+err.Error())
		return
	}
	if qty <= 0 {
		m.finalize(pos, 0, reason+" (already flat)")
		return
	}
	if qty > pos.Qty {
		// DEDICATED account (strict one desk per account since 2026-07-16): every share
		// of this symbol here is ours, so an exit flattens the FULL account quantity.
		// (The old only-sell-tracked-qty guard stranded leaked shares overnight on
		// 2026-07-17 — 16 unprotected positions.) Excess means entry-tracking leaked;
		// say so in the journal.
		m.journal(pos.Strategy, "error", pos.Symbol,
			fmt.Sprintf("exit found %.0f held vs %.0f tracked — flattening ALL (dedicated account)", qty, pos.Qty))
	}
	coid := fmt.Sprintf("ridp_%s__%s__exit__%s__%d", pos.Strategy, pos.Symbol, sanitize(reason), time.Now().UnixNano())
	id, serr := m.broker.MarketSell(pos.Symbol, qty, coid)
	if serr != nil {
		m.journal(pos.Strategy, "error", pos.Symbol, "exit failed: "+serr.Error())
		return
	}
	// The sell is ACCEPTED: the position is now CLOSING and stays on the books until
	// the account confirms flat (fast path below; otherwise resolveClosing next ticks).
	pos.ExitID, pos.ExitReason = id, reason
	m.saveState()
	fq, ap := m.awaitFill(id, 12*time.Second)
	if fq > 0 {
		if q2, qerr := m.broker.PositionQty(pos.Symbol); qerr == nil && q2 <= 0 {
			m.finalize(pos, ap, reason) // confirmed flat — the common fast path
			return
		}
	}
	m.journal(pos.Strategy, "exit", pos.Symbol,
		"exit accepted, awaiting flat confirmation — position remains tracked until the account confirms")
}

// finalize records the closed trade (exitPx 0 = mark at last price) and releases state.
func (m *Manager) finalize(pos *Position, exitPx float64, reason string) {
	if exitPx <= 0 {
		exitPx = m.lastPrice(pos.Symbol)
	}
	tr := Trade{Strategy: pos.Strategy, Symbol: pos.Symbol, Qty: pos.Qty, Entry: pos.Entry,
		Exit: exitPx, PnL: round2((exitPx - pos.Entry) * pos.Qty), Reason: reason,
		OpenedAt: pos.OpenedAt, ClosedAt: time.Now()}
	m.mu.Lock()
	delete(m.open, pos.Symbol)
	m.closed = append(m.closed, tr)
	m.lastExit[pos.Symbol] = time.Now() // re-entry cooldown: never buy into our own in-flight exit
	m.mu.Unlock()
	m.saveState()
	m.appendTrade(tr)
	m.journal(pos.Strategy, "exit", pos.Symbol,
		fmt.Sprintf("sold %.0f @ $%.2f — %s — P&L $%.2f", pos.Qty, exitPx, reason, tr.PnL))
	log.Printf("[ridp] %s EXIT %s @ $%.2f (%s) P&L $%.2f", pos.Strategy, pos.Symbol, exitPx, reason, tr.PnL)
}

// exchangeClosed reports whether a position was closed on the exchange (protective
// order filled, or a manual liquidation). It reads the BATCHED per-tick snapshot — no
// per-position API calls — and only when the shares are confirmed gone does it make one
// Order lookup to record the stop's real fill price. fillPx 0 = unknown (finalize marks
// at the last engine price). A missing/stale snapshot defers the decision to next tick.
func (m *Manager) exchangeClosed(p *Position) (closed bool, fillPx float64) {
	qty, ok := m.liveQty(p.Symbol)
	if !ok || qty > 0 {
		return false, 0
	}
	if p.StopID != "" {
		if fq, ap, st, err := m.broker.Order(p.StopID); err == nil && st == "filled" && fq > 0 {
			return true, ap
		}
	}
	return true, 0
}

// ensureProtection places the strategy's exchange-side protective order for a tracked
// position that doesn't have one (entry-time placement failed, e.g. under rate
// limiting). Retried every tick until it lands; software exits manage the position
// meanwhile, so it is never silently unmanaged.
func (m *Manager) ensureProtection(p *Position) {
	if p.StopID != "" {
		return
	}
	var id string
	var err error
	if p.Strategy == "rider" {
		sc := fmt.Sprintf("ridp_rider__%s__trail__%d", p.Symbol, time.Now().UnixNano())
		id, err = m.broker.TrailingStopSell(p.Symbol, p.Qty, riderTrailPct*100, sc)
	} else {
		stopPx := p.HardStop
		if stopPx <= 0 {
			stopPx = round2(p.Entry * 0.97) // defensive fallback; HardStop is always set on entry
		}
		sc := fmt.Sprintf("ridp_%s__%s__stop__%d", p.Strategy, p.Symbol, time.Now().UnixNano())
		id, err = m.broker.StopSell(p.Symbol, p.Qty, stopPx, sc)
	}
	if err != nil {
		return // retry next tick
	}
	p.StopID = id
	m.saveState()
	m.journal(p.Strategy, "entry", p.Symbol, "protection placed on retry: "+id)
	log.Printf("[ridp] %s %s protected on retry (%s)", p.Strategy, p.Symbol, id)
}

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

// ---- daily context ----

func (m *Manager) refreshDaily() {
	if m.dailyFn == nil {
		return
	}
	bars, err := m.dailyFn(m.symbols, 30)
	if err != nil {
		log.Printf("ridp: daily refresh error: %v", err)
		return
	}
	today := time.Now().In(m.etz).Format("2006-01-02")
	m.mu.Lock()
	defer m.mu.Unlock()
	for sym, db := range bars {
		// use only completed sessions (drop today's forming bar)
		var d []DailyBar
		for _, b := range db {
			if b.Day < today {
				d = append(d, b)
			}
		}
		n := len(d)
		if n < 16 {
			continue
		}
		var trs, vols []float64
		for i := 1; i < n; i++ {
			tr := d[i].High - d[i].Low
			if x := math.Abs(d[i].High - d[i-1].Close); x > tr {
				tr = x
			}
			if x := math.Abs(d[i-1].Close - d[i].Low); x > tr {
				tr = x
			}
			trs = append(trs, tr)
			vols = append(vols, d[i].Volume)
		}
		ctx := &dailyCtx{ATR: avgLast(trs, 14), AvgVol: avgLast(vols, 14), PrevHigh: d[n-1].High, AsOf: today}
		// DIPPER setup as of yesterday: 3 consecutive reds or -6% over 5 sessions,
		// evaluated on the sessions BEFORE the potential trigger day.
		if n >= 7 {
			reds := 0
			for i := n - 2; i > 0 && d[i].Close < d[i-1].Close; i-- {
				reds++
			}
			drop5 := d[n-2].Close/d[n-7].Close - 1
			setup := reds >= dipperRedDays || drop5 <= dipperDrop5d
			// trigger: yesterday CLOSED above the prior day's high, out of a setup.
			// Throughput mode adds an alternate turn: closed >= +dipperTurnPct over the
			// prior close on above-average volume (the original prior-high bar rarely
			// prints after a real multi-day fall; 15 radar names, 0 triggers in 2 weeks).
			trig := setup && d[n-1].Close > d[n-2].High
			if !trig && setup && dipperTurnPct > 0 {
				trig = d[n-1].Close >= d[n-2].Close*(1+dipperTurnPct) && ctx.AvgVol > 0 && d[n-1].Volume > ctx.AvgVol
			}
			ctx.Setup = setup
			ctx.Triggered = trig
		}
		m.daily[sym] = ctx
	}
	m.dailyDay = today
	log.Printf("ridp: daily context refreshed for %d symbols (%s)", len(m.daily), today)
}

// ---- persistence & rehydration ----

func (m *Manager) statePath() string { return filepath.Join(m.dataDir, "state.json") }

func (m *Manager) saveState() {
	m.mu.Lock()
	b, _ := json.MarshalIndent(m.open, "", " ")
	m.mu.Unlock()
	// ATOMIC write (temp + rename): a crash mid-write must never corrupt the books —
	// the ghost reconciler SELLS anything the books don't claim, so corrupt books that
	// load empty would flatten legitimate overnight positions (e.g. DIPPER's).
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("ridp: state save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		log.Printf("ridp: state rename failed: %v", err)
	}
}

func (m *Manager) loadState() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return // no file = genuinely empty books (fresh start) — reconcile may act
	}
	if err := json.Unmarshal(b, &m.open); err != nil {
		// File EXISTS but is unreadable: the books are UNRELIABLE, not empty. Fail safe:
		// the ghost reconciler must not sell anything it can't prove is untracked.
		m.booksBad = true
		log.Printf("ridp: ⚠ CRITICAL: state.json is corrupt (%v) — ghost reconcile DISABLED until the file is fixed or removed; positions on the account are NOT being auto-flattened", err)
	}
	if m.open == nil {
		m.open = map[string]*Position{}
	}
	// closed trades (for the report) — today + yesterday's tail
	if f, err := os.Open(filepath.Join(m.dataDir, "trades.jsonl")); err == nil {
		dec := json.NewDecoder(f)
		for {
			var t Trade
			if dec.Decode(&t) != nil {
				break
			}
			m.closed = append(m.closed, t)
		}
		f.Close()
		if len(m.closed) > 200 {
			m.closed = m.closed[len(m.closed)-200:]
		}
	}
}

// rehydrate verifies persisted open positions against the broker after a restart.
func (m *Manager) rehydrate() {
	m.refreshLivePositions() // seed the snapshot so offline closes reconcile immediately
	m.mu.Lock()
	positions := make([]*Position, 0, len(m.open))
	for _, p := range m.open {
		positions = append(positions, p)
	}
	m.mu.Unlock()
	for _, p := range positions {
		if closed, px := m.exchangeClosed(p); closed {
			m.finalize(p, px, "closed while offline (protective order)")
			continue
		}
		if _, err := m.broker.PositionQty(p.Symbol); err != nil {
			continue // transient; keep managing, protective order is on the exchange
		}
		if m.ensureLive != nil {
			m.ensureLive(p.Symbol)
		}
		log.Printf("[ridp] rehydrated %s %s %.0f @ $%.2f", p.Strategy, p.Symbol, p.Qty, p.Entry)
	}
}

// ---- journal & helpers ----

// journalSkip journals an allocator skip at most once per (strategy, symbol) per 5
// minutes. Skips re-evaluate on every 10-second tick and were flooding the day journal
// (6,414 identical budget lines on 2026-07-13) without adding information. Trading
// behavior is unchanged — only the logging is throttled.
func (m *Manager) journalSkip(strategy, sym, note string) {
	key := strategy + "|" + sym
	now := time.Now()
	m.mu.Lock()
	if last, ok := m.lastSkip[key]; ok && now.Sub(last) < 5*time.Minute {
		m.mu.Unlock()
		return
	}
	m.lastSkip[key] = now
	m.mu.Unlock()
	m.journal(strategy, "skip", sym, note)
}

func (m *Manager) journal(strategy, event, sym, note string) {
	rec := map[string]interface{}{
		"time": time.Now().In(m.etz).Format(time.RFC3339), "strategy": strategy,
		"event": event, "symbol": sym, "note": note,
	}
	b, _ := json.Marshal(rec)
	day := time.Now().In(m.etz).Format("2006-01-02")
	f, err := os.OpenFile(filepath.Join(m.dataDir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func (m *Manager) appendTrade(t Trade) {
	b, _ := json.Marshal(t)
	f, err := os.OpenFile(filepath.Join(m.dataDir, "trades.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func (m *Manager) lastPrice(sym string) float64 {
	bars := m.engine.Snapshot(sym, 1)
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].Close
}

func avgLast(v []float64, n int) float64 {
	if len(v) == 0 {
		return 0
	}
	if len(v) > n {
		v = v[len(v)-n:]
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else if r == ' ' {
			out = append(out, '_')
		}
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return string(out)
}

// ---- report (for /api/ridp) ----

type PositionView struct {
	Position
	Last       float64 `json:"last"`
	Unrealized float64 `json:"unrealized"`
	TrailLevel float64 `json:"trail_level"`
}

type StratStats struct {
	Trades   int     `json:"trades"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"win_rate"`
	Realized float64 `json:"realized_pnl"`
	AvgPnL   float64 `json:"avg_pnl"`
	Today    float64 `json:"today_pnl"`
}

type Report struct {
	Enabled    bool           `json:"enabled"`
	Live       bool           `json:"live"`
	Equity     float64        `json:"account_equity"`
	LastEquity float64        `json:"account_last_equity"` // Alpaca: equity at prior close
	DayPnL     float64        `json:"account_day_pnl"`     // Alpaca: equity − last_equity (broker truth, ghosts included)
	Buying     float64        `json:"buying_power"`
	Deployed   float64        `json:"deployed"`
	Open         []PositionView `json:"open"`
	Rider        StratStats     `json:"rider"`
	Dipper       StratStats     `json:"dipper"`
	Setups       []string       `json:"dipper_setups"`    // symbols in a qualified falling setup
	Triggered    []string       `json:"dipper_triggered"` // setups that fired the turn signal
	Closed       []Trade        `json:"closed"`
	Symbols      int            `json:"universe_size"`
	ReverterOpen []PositionView `json:"reverter_open"` // REVERTER shadow virtual positions
	Reverter     StratStats     `json:"reverter"`      // REVERTER shadow realized stats
	Ghosts       []GhostView    `json:"ghosts"`        // on the ACCOUNT but tracked by no strategy
	BooksBad     bool           `json:"books_bad"`     // state.json corrupt — reconcile standing down
}

// GhostView is an account holding no strategy claims — straight from the Alpaca
// snapshot, so the page can never show "empty" while the account holds stock.
type GhostView struct {
	Symbol string  `json:"symbol"`
	Qty    float64 `json:"qty"` // untracked share count (account − tracked)
	Last   float64 `json:"last"`
}

func (m *Manager) Report() Report {
	r := Report{Enabled: m.Enabled(), Live: m.live, Symbols: len(m.symbols)}
	if ai, err := m.account(); err == nil {
		r.Equity, r.Buying = ai.Equity, ai.BuyingPower
		r.LastEquity, r.DayPnL = ai.LastEquity, round2(ai.DayPnL())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.open {
		last := m.lastPrice(p.Symbol)
		trail := 0.0
		switch p.Strategy {
		case "rider":
			k := riderTrailPct
			if p.Tightened {
				k = riderTightPct
			}
			trail = p.Peak * (1 - k)
		case "reverter":
			trail = p.HardStop // the z=-4 exchange floor (software exit is the moving mean)
		default:
			trail = p.Peak - dipperTrailATR*p.ATR
			if p.HardStop > trail {
				trail = p.HardStop
			}
		}
		r.Deployed += p.Qty * p.Entry
		view := PositionView{Position: *p, Last: last,
			Unrealized: round2((last - p.Entry) * p.Qty), TrailLevel: round2(trail)}
		if p.Strategy == "reverter" {
			r.ReverterOpen = append(r.ReverterOpen, view)
		} else {
			r.Open = append(r.Open, view)
		}
	}
	sort.Slice(r.Open, func(i, j int) bool { return r.Open[i].OpenedAt.Before(r.Open[j].OpenedAt) })
	sort.Slice(r.ReverterOpen, func(i, j int) bool { return r.ReverterOpen[i].OpenedAt.Before(r.ReverterOpen[j].OpenedAt) })
	// Broker-truth cross-check: anything on the account that no strategy tracks shows
	// up as a ghost row (fresh snapshot only; qty is the untracked EXCESS per symbol).
	r.BooksBad = m.booksBad
	if !m.livePosAt.IsZero() && time.Since(m.livePosAt) < 2*time.Minute {
		for sym, qty := range m.livePos {
			tracked := 0.0
			if p, ok := m.open[sym]; ok {
				tracked = p.Qty
			}
			if excess := qty - tracked; excess >= 1 {
				r.Ghosts = append(r.Ghosts, GhostView{Symbol: sym, Qty: excess, Last: m.lastPrice(sym)})
			}
		}
		sort.Slice(r.Ghosts, func(i, j int) bool { return r.Ghosts[i].Symbol < r.Ghosts[j].Symbol })
	}
	today := time.Now().In(m.etz).Format("2006-01-02")
	agg := func(strategy string) StratStats {
		st := StratStats{}
		for _, t := range m.closed {
			if t.Strategy != strategy {
				continue
			}
			st.Trades++
			st.Realized += t.PnL
			if t.PnL > 0 {
				st.Wins++
			}
			if t.ClosedAt.In(m.etz).Format("2006-01-02") == today {
				st.Today += t.PnL
			}
		}
		if st.Trades > 0 {
			st.WinRate = float64(st.Wins) / float64(st.Trades)
			st.AvgPnL = round2(st.Realized / float64(st.Trades))
		}
		st.Realized = round2(st.Realized)
		st.Today = round2(st.Today)
		return st
	}
	r.Rider, r.Dipper, r.Reverter = agg("rider"), agg("dipper"), agg("reverter")
	for sym, d := range m.daily {
		if d.Triggered {
			r.Triggered = append(r.Triggered, sym)
		} else if d.Setup {
			r.Setups = append(r.Setups, sym)
		}
	}
	sort.Strings(r.Setups)
	sort.Strings(r.Triggered)
	n := len(m.closed)
	if n > 50 {
		r.Closed = append([]Trade(nil), m.closed[n-50:]...)
	} else {
		r.Closed = append([]Trade(nil), m.closed...)
	}
	for i, j := 0, len(r.Closed)-1; i < j; i, j = i+1, j-1 {
		r.Closed[i], r.Closed[j] = r.Closed[j], r.Closed[i]
	}
	return r
}
