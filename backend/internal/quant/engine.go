package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/risk"
	"live-optimus/backend/internal/scanner"
)

// DipInput is the raw dip signal handed in by the detector (dipwatch). The engine enriches it
// into a full DipEvent using the shared candle engine.
type DipInput struct {
	Symbol       string
	Time         time.Time
	Price        float64
	DayHigh      float64
	DipLow       float64
	ATR          float64
	RSI          float64
	VWAP         float64
	DayOpen      float64
	RVOL         float64
	BounceVolume float64
	NegativeNews bool
}

// Engine ties the quant pipeline together. PHASE 2 runs in SHADOW mode: on each detected dip it
// asks Agent 2 (entry), logs the structured decision, and returns a label for the Telegram alert
// — but it does NOT place orders yet (no position is opened before its exit manager, Agent 3,
// exists). When `live` is enabled (later phase) approved buys go to the allocator + paper order.
type Engine struct {
	universe *Universe
	alloc    *Allocator
	log      *DecisionLog
	agent2   *Agent2
	candles  *candles.Engine
	scanner  *scanner.Scanner
	agent4   *Agent4
	broker   *Broker
	manager  *Manager
	rise     *RiseWatch // optional: arms declined dips for rise confirmation
	loc      *time.Location
	ctx      context.Context
	dataDir  string
	live     bool
	dayRisk  *risk.Day // dip+rise desk's daily loss cap (nil = no cap on dip entries)

	// The SIGNAL desk's stack, referenced for reporting only (the signal trader owns the
	// trading path). The engine's own broker/alloc/manager above belong to the DIP+RISE
	// desk — the two families run on separate paper accounts with separate budgets.
	sigBroker *Broker
	sigAlloc  *Allocator
	sigLive   bool

	// Funding coordinator: co-arriving approved buys (dips confirmed in the same burst, each on
	// its own goroutine) are gathered for a short window, then funded HIGHEST dip-quality first
	// under the shared budget — not first-come. Guards a single in-flight round.
	fundMu     sync.Mutex
	fundRound  []*pendingBuy
	fundActive bool
}

// fundGatherWindow is how long an approved buy waits for other approved buys to arrive before the
// round is ranked and funded. Short enough not to delay entries meaningfully; long enough to catch
// dips confirmed in the same scan burst whose Agent-2 calls finish close together.
const fundGatherWindow = 2 * time.Second

// pendingBuy is one Agent-2-approved buy waiting in the current funding round.
type pendingBuy struct {
	cand   Candidate
	result chan fundResult
}

type fundResult struct {
	size   float64 // funded dollars (0 = not funded)
	reason string  // why size is 0 (for the alert/log)
}

// SetDataDir sets the base data directory (for reading daily reviews in Report).
func (e *Engine) SetDataDir(d string) { e.dataDir = d }

// SetAgent4 wires the sentiment agent (optional; its cached score enriches Agent 2's snapshot).
func (e *Engine) SetAgent4(a *Agent4) { e.agent4 = a }

// SetRiseWatch wires the rising watcher: dips Agent 2 declines get armed for a short
// window and entered only if the rise CONFIRMS (a validated, deterministic second chance).
func (e *Engine) SetRiseWatch(r *RiseWatch) { e.rise = r }

// SetContext provides the app context used for per-position management goroutines.
func (e *Engine) SetContext(ctx context.Context) { e.ctx = ctx }

// SetExecution wires the DIP+RISE desk's paper broker + exit manager. Once set AND
// SetLive(true), approved dip buys place real paper orders (with the deterministic stop
// floor) and Agent 3 manages the exit.
func (e *Engine) SetExecution(b *Broker, m *Manager) {
	e.broker = b
	e.manager = m
}

// SetSignalExecution registers the SIGNAL desk's broker + allocator for reporting (the
// signal trader owns that desk's trading path directly).
func (e *Engine) SetSignalExecution(b *Broker, a *Allocator, live bool) {
	e.sigBroker = b
	e.sigAlloc = a
	e.sigLive = live
}

// SetDayRisk wires the dip+rise desk's daily loss-cap tracker: once the desk has lost
// its cap for the day, approved dip buys are skipped until tomorrow (protection only —
// it never places or changes orders).
func (e *Engine) SetDayRisk(d *risk.Day) { e.dayRisk = d }

// SentimentScore returns Agent 4's cached score for a symbol (0 if none) — for allocator ranking.
func (e *Engine) sentimentScore(sym string) float64 {
	if e.agent4 == nil {
		return 0
	}
	if s, ok := e.agent4.Get(sym); ok {
		return s.Score
	}
	return 0
}

func NewEngine(u *Universe, alloc *Allocator, dlog *DecisionLog, agent2 *Agent2, eng *candles.Engine, scn *scanner.Scanner, loc *time.Location) *Engine {
	if loc == nil {
		loc = time.UTC
	}
	return &Engine{universe: u, alloc: alloc, log: dlog, agent2: agent2, candles: eng, scanner: scn, loc: loc, live: false}
}

// SetLive enables real order placement (kept false until Agent 3 / the exit manager is wired).
func (e *Engine) SetLive(v bool) { e.live = v }

// OnDip is called by the detector for every confirmed dip (whole watchlist). It returns a short
// label describing what the agent did, which the detector appends to the Telegram alert so you
// always know whether the agent acted and what it decided.
func (e *Engine) OnDip(in DipInput) string {
	sym := strings.ToUpper(strings.TrimSpace(in.Symbol))

	// Only the curated daily universe is actionable by the agents (the bot still alerts broadly).
	if e.universe == nil || !e.universe.Has(sym) {
		e.logRec(LogRecord{Agent: "pipeline", Event: "skip", Symbol: sym, Note: "not in today's universe"})
		return "👁 FYI — not in today's agent universe"
	}
	if e.universe.StandDown() {
		e.logRec(LogRecord{Agent: "pipeline", Event: "skip", Symbol: sym, Note: "regime stand_down"})
		return "🛑 stand-down (no entries today)"
	}

	de := e.buildDipEvent(in)
	e.logRec(LogRecord{Agent: "pipeline", Event: "dip", Symbol: sym, Output: de})

	if e.agent2 == nil || !e.agent2.Enabled() {
		return "🤖 agent idle (set ANTHROPIC_API_KEY)"
	}

	snap := e.entrySnapshot(sym, de)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	dec, usage, err := e.agent2.Decide(ctx, snap)
	if err != nil {
		e.logRec(LogRecord{Agent: "agent2_entry", Event: "error", Symbol: sym, Note: err.Error()})
		log.Printf("[quant] agent2 %s error: %v", sym, err)
		return "🤖 Agent 2 error (logged)"
	}
	e.logRec(LogRecord{Agent: "agent2_entry", Event: "decision", Symbol: sym, Model: e.agent2.model,
		Input: json.RawMessage(snap), Output: dec, Tokens: &usage})
	log.Printf("[quant] agent2 %s -> %s (%.2f): %s", sym, dec.Action, dec.Confidence, dec.Reason)

	if !dec.IsBuy() {
		// Second chance: the replay showed most declined dips are correctly declined AT
		// DETECTION, but many still bounce — arm the rise watcher to catch the confirmed ones.
		if e.rise != nil && e.rise.Arm(de, dec.Confidence) {
			return fmt.Sprintf("🤖 Agent 2: NO-BUY — %s\n⏱ rise-watch armed (%dm): will buy only a confirmed rise", dec.Reason, riseWatchWindowMin)
		}
		return fmt.Sprintf("🤖 Agent 2: NO-BUY — %s", dec.Reason)
	}

	// Approved buy. Shadow mode reports what it WOULD do (advisory size, no capital committed).
	if !e.live || e.manager == nil || e.alloc == nil {
		size := 0.0
		if e.alloc != nil {
			size = e.alloc.Size(dec.Confidence)
		}
		e.logRec(LogRecord{Agent: "allocator", Event: "skip", Symbol: sym,
			Note: fmt.Sprintf("SHADOW: would fund $%.0f (live trading off)", size)})
		return fmt.Sprintf("🤖 Agent 2: BUY (%.2f) → would fund $%.0f [shadow] — %s", dec.Confidence, size, dec.Reason)
	}

	// Daily loss cap (the desk's "bad day? stop digging" brake): once today's realized
	// P&L hits −cap, approved buys are skipped until tomorrow. Same protection the
	// signal desk has; open positions keep being managed normally.
	if e.dayRisk != nil {
		if err := e.dayRisk.CanEnter(e.alloc.OpenCount(), time.Now()); err != nil {
			e.logRec(LogRecord{Agent: "allocator", Event: "skip", Symbol: sym, Note: err.Error()})
			return fmt.Sprintf("🤖 Agent 2: BUY (%.2f) — skipped: %s", dec.Confidence, err.Error())
		}
	}

	// LIVE: hand the approved buy to the funding coordinator, which gathers co-arriving approved
	// buys and funds the HIGHEST dip-quality first under the shared budget (not first-come). It
	// reserves the capital on success; the manager then runs the entry → deterministic stop →
	// Agent 3 exit. Outranked / unaffordable buys are skipped and logged, never force-funded.
	tier := 0
	if ent, ok := e.universe.Entry(sym); ok {
		tier = ent.Tier
	}
	size, reason := e.fundContended(Candidate{
		Symbol: sym, Confidence: dec.Confidence, Dip: de, Tier: tier, Sentiment: e.sentimentScore(sym),
	})
	if size <= 0 {
		e.logRec(LogRecord{Agent: "allocator", Event: "skip", Symbol: sym, Note: reason})
		return fmt.Sprintf("🤖 Agent 2: BUY (%.2f) — %s, skipped", dec.Confidence, reason)
	}
	e.logRec(LogRecord{Agent: "allocator", Event: "order", Symbol: sym,
		Note: fmt.Sprintf("funded $%.0f (conf %.2f)", size, dec.Confidence)})
	appCtx := e.ctx
	if appCtx == nil {
		appCtx = context.Background()
	}
	go e.manager.Open(appCtx, de, dec.Confidence, size)
	return fmt.Sprintf("🤖 Agent 2: BUY (%.2f) → entering $%.0f — %s", dec.Confidence, size, dec.Reason)
}

// fundContended enters an approved buy into the current funding round and blocks until the round
// is resolved, returning the funded dollar size (0 + a reason if outranked or unaffordable). The
// caller is already on its own goroutine (the dip hook), so the short wait is harmless and it
// preserves the synchronous labeled-alert contract.
func (e *Engine) fundContended(c Candidate) (float64, string) {
	pb := &pendingBuy{cand: c, result: make(chan fundResult, 1)}
	e.fundMu.Lock()
	e.fundRound = append(e.fundRound, pb)
	if !e.fundActive {
		e.fundActive = true
		go e.flushFundRound()
	}
	e.fundMu.Unlock()
	res := <-pb.result
	return res.size, res.reason
}

// flushFundRound waits the gather window, then ranks the round best-first and funds in order so the
// highest-quality dip gets first claim on the limited slots/capital.
func (e *Engine) flushFundRound() {
	time.Sleep(fundGatherWindow)
	e.fundMu.Lock()
	round := e.fundRound
	e.fundRound = nil
	e.fundActive = false
	e.fundMu.Unlock()

	sort.SliceStable(round, func(i, j int) bool { return Score(round[i].cand) > Score(round[j].cand) })
	for _, pb := range round {
		size, reason := e.tryFund(pb.cand)
		pb.result <- fundResult{size: size, reason: reason}
	}
}

// tryFund reserves capital for one candidate (called in rank order). Whole-share: if the funded
// size can't buy even one share, it skips cleanly without reserving capital.
func (e *Engine) tryFund(c Candidate) (float64, string) {
	if !e.alloc.CanFund(c.Symbol) {
		return 0, "no slot/capital free"
	}
	size := e.alloc.Size(c.Confidence)
	if size <= 0 {
		return 0, "no capital free"
	}
	px := e.LastClose(c.Symbol)
	if px <= 0 || math.Floor(size/px) < 1 {
		return 0, fmt.Sprintf("$%.0f < 1 share", size)
	}
	if !e.alloc.Fund(c.Symbol, size) {
		return 0, "slot taken"
	}
	return size, ""
}

// LastClose returns the latest 1-min close for a symbol (0 if none).
func (e *Engine) LastClose(sym string) float64 {
	snap := e.candles.Snapshot(sym, 1)
	if len(snap) == 0 {
		return 0
	}
	return snap[len(snap)-1].Close
}

// exitSnapshot builds the JSON Agent 3 sees for an open position. It includes the entry
// PLAN (strategy + original target/stop) so the exit agent can manage the trade knowing
// its goal — a mean-reversion dip vs a momentum breakout want different handling.
func (e *Engine) exitSnapshot(sym string, pos *managedPos, entryPrice, qty, curStop float64, entryTime time.Time) string {
	cur := e.LastClose(sym)
	pnlPct := 0.0
	if entryPrice > 0 {
		pnlPct = (cur - entryPrice) / entryPrice * 100
	}
	_, _, vwap := e.sessionAgg(sym)
	pvw := 0.0
	if vwap > 0 {
		pvw = (cur - vwap) / vwap * 100
	}
	rvol := 0.0
	if e.scanner != nil {
		if st, ok := e.scanner.Get(sym); ok {
			rvol = st.RVOL
		}
	}
	snap := map[string]interface{}{
		"now_et":             time.Now().In(e.loc).Format("15:04"),
		"symbol":             sym,
		"entry_price":        round2(entryPrice),
		"current_price":      round2(cur),
		"qty":                qty,
		"unrealized_pnl_pct": round2(pnlPct),
		"minutes_held":       round2(time.Since(entryTime).Minutes()),
		"current_stop":       round2(curStop),
		"price_vs_vwap_pct":  round2(pvw),
		"rsi":                round2(rsi1m(e.candles.Snapshot(sym, 1), 14)),
		"rvol":               round2(rvol),
		"market":             e.marketContext(),
		"bars_1m":            e.recentBars(sym, 10),
		// Two granularities on purpose: 10x1-min for fine recent detail, 4x5-min for the
		// last ~20 minutes of structure (trend, higher lows) — wider context at almost no
		// extra token cost vs doubling the 1-min window.
		"bars_5m": e.recentBarsTF(sym, 5, 4),
	}
	// Entry PLAN — so Agent 3 manages with knowledge of intent, not blind.
	if pos != nil {
		if pos.strategy != "" {
			snap["strategy"] = pos.strategy
		}
		snap["conviction"] = round2(pos.conf)
		if pos.origStop > 0 {
			snap["original_stop"] = round2(pos.origStop)
		}
		if pos.target > 0 {
			snap["original_target"] = round2(pos.target)
			// How far from entry to the planned exit we've traveled (100% = target hit).
			if entryPrice > 0 && pos.target > entryPrice {
				snap["pct_to_target"] = round2((cur - entryPrice) / (pos.target - entryPrice) * 100)
			}
		}
	}
	b, _ := json.Marshal(snap)
	return string(b)
}

func (e *Engine) recentBars(sym string, n int) []map[string]interface{} {
	return e.recentBarsTF(sym, 1, n)
}

// recentBarsTF returns the last n bars of the given timeframe (minutes) for the snapshot.
func (e *Engine) recentBarsTF(sym string, tf, n int) []map[string]interface{} {
	bars := e.candles.Snapshot(sym, tf)
	if n > 0 && len(bars) > n {
		bars = bars[len(bars)-n:]
	}
	out := make([]map[string]interface{}, 0, len(bars))
	for _, b := range bars {
		out = append(out, map[string]interface{}{
			"t": time.Unix(b.Time, 0).In(e.loc).Format("15:04"),
			"o": round2(b.Open), "h": round2(b.High), "l": round2(b.Low), "c": round2(b.Close), "v": b.Volume,
		})
	}
	return out
}

func (e *Engine) logRec(r LogRecord) {
	if e.log != nil {
		e.log.Append(r)
	}
}

func (e *Engine) sessionStartUnix() int64 {
	n := time.Now().In(e.loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, e.loc).Unix()
}

// buildDipEvent enriches the raw signal with the dip's anatomy (duration, shape, dip volume)
// computed from the 1-minute session bars.
func (e *Engine) buildDipEvent(in DipInput) DipEvent {
	de := DipEvent{
		Symbol: strings.ToUpper(in.Symbol), DetectedAt: in.Time, Price: round2(in.Price),
		PreDipHigh: round2(in.DayHigh), DipLow: round2(in.DipLow), RVOL: round2(in.RVOL),
		RSI: round2(in.RSI), VWAP: round2(in.VWAP), BounceVolume: in.BounceVolume,
	}
	if in.DayHigh > 0 {
		de.DepthPct = round2((in.DayHigh - in.DipLow) / in.DayHigh * 100)
	}
	if in.ATR > 0 {
		de.DepthATR = round2((in.DayHigh - in.DipLow) / in.ATR)
	}
	if in.VWAP > 0 {
		de.PriceVsVWAP = round2((in.Price - in.VWAP) / in.VWAP * 100)
	}

	// Walk the session 1-min bars: find the high bar, then the low after it (the dip), and sum
	// the volume across the drop. Shape = how fast the drop was.
	start := e.sessionStartUnix()
	bars := e.candles.Snapshot(in.Symbol, 1)
	highIdx, lowIdx := -1, -1
	var highT, lowT int64
	hi := math.Inf(-1)
	for i, b := range bars {
		if b.Time < start {
			continue
		}
		if b.High >= hi {
			hi = b.High
			highIdx = i
			highT = b.Time
		}
	}
	if highIdx >= 0 {
		lo := math.Inf(1)
		for i := highIdx; i < len(bars); i++ {
			if bars[i].Low <= lo {
				lo = bars[i].Low
				lowIdx = i
				lowT = bars[i].Time
			}
		}
	}
	if highIdx >= 0 && lowIdx >= highIdx {
		de.DurationMin = round2(float64(lowT-highT) / 60)
		var dv float64
		for i := highIdx; i <= lowIdx; i++ {
			dv += bars[i].Volume
		}
		de.DipVolume = dv
		if de.DurationMin <= 10 {
			de.Shape = "sharp_v"
		} else {
			de.Shape = "grinding"
		}
	}
	return de
}

// entrySnapshot assembles the JSON Agent 2 sees: the dip anatomy + market backdrop + today's
// universe metadata + market posture.
func (e *Engine) entrySnapshot(sym string, de DipEvent) string {
	snap := map[string]interface{}{
		"now_et": time.Now().In(e.loc).Format("15:04"),
		"dip":    de,
		"market": e.marketContext(),
		"regime": e.universe.Regime(),
	}
	if ent, ok := e.universe.Entry(sym); ok {
		snap["universe"] = map[string]interface{}{
			"tier": ent.Tier, "catalyst": ent.Catalyst,
			"sentiment_lean": ent.SentimentLean, "risk_flags": ent.RiskFlags,
		}
	}
	// Live sentiment from Agent 4 (local model) overrides the static pre-market lean when present.
	if e.agent4 != nil {
		if s, ok := e.agent4.Get(sym); ok {
			snap["sentiment"] = map[string]interface{}{
				"lean": s.Lean, "score": round2(s.Score), "has_catalyst": s.Catalyst, "why": s.Why,
			}
		}
	}
	b, _ := json.Marshal(snap)
	return string(b)
}

// marketContext returns the SPY/QQQ backdrop (% from open + above/below VWAP).
func (e *Engine) marketContext() map[string]interface{} {
	out := map[string]interface{}{}
	for _, s := range []string{"SPY", "QQQ"} {
		o, last, vwap := e.sessionAgg(s)
		pct := 0.0
		if o > 0 && last > 0 {
			pct = (last - o) / o * 100
		}
		p := strings.ToLower(s)
		out[p+"_pct_from_open"] = round2(pct)
		out[p+"_above_vwap"] = vwap > 0 && last >= vwap
	}
	return out
}

// sessionAgg returns today's regular-session open, latest price, and VWAP for a symbol.
func (e *Engine) sessionAgg(sym string) (open, last, vwap float64) {
	start := e.sessionStartUnix()
	var pv, vol float64
	first := true
	for _, b := range e.candles.Snapshot(sym, 1) {
		if b.Time < start || b.Volume <= 0 {
			continue
		}
		if first {
			open = b.Open
			first = false
		}
		pv += (b.High + b.Low + b.Close) / 3 * b.Volume
		vol += b.Volume
		last = b.Close
	}
	if vol > 0 {
		vwap = pv / vol
	}
	return
}

// AllocSnapshot exposes the allocator state for the API/page.
func (e *Engine) AllocSnapshot() AllocSnapshot {
	if e.alloc == nil {
		return AllocSnapshot{}
	}
	return e.alloc.Snapshot()
}

// Configure (re)applies today's allocation parameters (call after a universe reload).
func (e *Engine) Configure() {
	if e.alloc != nil && e.universe != nil {
		e.alloc.Configure(e.universe.Allocation())
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// rsi1m computes Wilder's RSI over a candle series' closes.
func rsi1m(bars []candles.Candle, length int) float64 {
	if len(bars) <= length {
		return 0
	}
	c := make([]float64, len(bars))
	for i, b := range bars {
		c[i] = b.Close
	}
	var gain, loss float64
	for i := 1; i <= length; i++ {
		d := c[i] - c[i-1]
		if d >= 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	ag, al := gain/float64(length), loss/float64(length)
	for i := length + 1; i < len(c); i++ {
		d := c[i] - c[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else if d < 0 {
			l = -d
		}
		ag = (ag*float64(length-1) + g) / float64(length)
		al = (al*float64(length-1) + l) / float64(length)
	}
	if al == 0 {
		return 100
	}
	return 100 - 100/(1+ag/al)
}

// ensure scanner import is used (reserved for richer snapshots in a later phase).
var _ = scanner.State{}
