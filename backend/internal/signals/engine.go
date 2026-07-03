package signals

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Engine is the LIVE signal scanner. It consumes completed 1-minute bars for the quant
// universe (fed off the existing SIP stream — an additive consumer, nothing on the
// execution page's path changes), runs every strategy on each new bar, and logs each
// published Signal plus its counterfactual bracket outcome to
// data/signals/YYYY-MM-DD.jsonl.
//
// SHADOW-FIRST: the engine places no orders. OnSignal (optional) is the future execution
// hook — it stays nil until backtesting validates the strategy set and the operator
// enables paper execution.
type Engine struct {
	store  *Store
	uni    *Universe
	strats []Strategy
	logDir string
	et     *time.Location

	// OnSignal, when set, receives every published signal (the paper-execution hook).
	// Set via SetOnSignal after construction — reads are synchronized with publishes.
	OnSignal func(Signal)

	// ExtraFeatures, when set, is called for every detected signal (before publish) to
	// merge live-only microstructure columns (spread, order flow) into sig.Features —
	// columns historical bars can't reconstruct, so they only exist in the live journal.
	// Set via SetExtraFeatures — reads are synchronized with publishes.
	ExtraFeatures func(sym string) map[string]float64

	mu       sync.Mutex
	cool     map[string]int64 // "strategy|symbol" -> last signal unix (30-min cooldown)
	dayCnt   map[string]int   // "strategy|symbol|day" -> signals published today
	pending  []*pendingOutcome
	seq      int64
	sweptDay string // last session day cool/dayCnt were swept for (bounds their growth)

	// Time-of-day conditioning (the promoted Tier-1 mechanism): per (strategy,
	// half-hour-of-session) running outcome stats, persisted across restarts and
	// warm-startable from a backtest-generated seed file. Signals fired in a bucket
	// with ≥ condMinSamples outcomes and negative mean R are flagged tod_blocked in the
	// journal, and EntryAllowed reports false for them (the future execution gate).
	tod     map[string]*todStat // "strategy|bucket"
	todPath string
}

// todStat is one persisted conditioning bucket.
type todStat struct {
	N   int     `json:"n"`
	Sum float64 `json:"sum"`
}

func (s *todStat) blocks() bool { return s.N >= condMinSamples && s.Sum/float64(s.N) < 0 }

// pendingOutcome tracks one published signal until its counterfactual bracket resolves.
type pendingOutcome struct {
	sig       Signal
	todBucket int
	done      bool
}

// signalCooldown / maxPerDay bound repeat-fires per (strategy, symbol).
const (
	signalCooldown = 30 * time.Minute
	maxPerDay      = 2
)

// NewEngine builds the live engine. dataDir is the backend data directory ("data").
func NewEngine(uni *Universe, dataDir string) *Engine {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	e := &Engine{
		store:   NewStore(),
		uni:     uni,
		strats:  DefaultStrategies(),
		logDir:  filepath.Join(dataDir, "signals"),
		et:      loc,
		cool:    map[string]int64{},
		dayCnt:  map[string]int{},
		tod:     map[string]*todStat{},
		todPath: filepath.Join(dataDir, "signals", "tod_stats.json"),
	}
	e.loadTOD()
	return e
}

// loadTOD restores the persisted conditioning stats (seeded by backtests, grown live).
func (e *Engine) loadTOD() {
	b, err := os.ReadFile(e.todPath)
	if err != nil {
		return
	}
	m := map[string]*todStat{}
	if json.Unmarshal(b, &m) == nil {
		e.tod = m
		log.Printf("[signals] time-of-day stats loaded: %d buckets", len(m))
	}
}

// saveTOD persists the conditioning stats (best-effort; caller holds e.mu).
func (e *Engine) saveTOD() {
	b, err := json.MarshalIndent(e.tod, "", " ")
	if err != nil {
		return
	}
	if os.MkdirAll(e.logDir, 0o755) == nil {
		_ = os.WriteFile(e.todPath, b, 0o644)
	}
}

// todBlocked reports whether a (strategy, bucket) pair has proven negative expectancy.
func (e *Engine) todBlocked(strategy string, bucket int) bool {
	if st := e.tod[strategy+"|"+strconv.Itoa(bucket)]; st != nil {
		return st.blocks()
	}
	return false
}

// EntryAllowed is the execution-time gate (used once paper execution is enabled; today
// it only annotates the shadow journal): true unless the signal's time-of-day bucket has
// proven negative expectancy.
func (e *Engine) EntryAllowed(sig Signal) bool {
	open := e.store.SessionOpen()
	if open == 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.todBlocked(sig.Strategy, minuteOf(sig.Time, open)/30)
}

// Universe exposes the loaded universe (for stream subscription wiring).
func (e *Engine) Universe() *Universe { return e.uni }

// SetOnSignal installs the execution hook (safe to call after the stream started).
func (e *Engine) SetOnSignal(fn func(Signal)) {
	e.mu.Lock()
	e.OnSignal = fn
	e.mu.Unlock()
}

// SetExtraFeatures installs the live microstructure-feature hook (safe to call after the
// stream started).
func (e *Engine) SetExtraFeatures(fn func(sym string) map[string]float64) {
	e.mu.Lock()
	e.ExtraFeatures = fn
	e.mu.Unlock()
}

// SectorLeadLag computes sym's live sector_ret_15m / peer_gap_15m (P2.1, RESEARCH_BACKLOG
// #9) from today's session bars. Meant to be folded into an ExtraFeatures hook (nil map
// if not yet computable — e.g. under 15 minutes into the session).
func (e *Engine) SectorLeadLag(sym string) map[string]float64 {
	return sectorLeadLagFeatures(e.uni, sym, e.store.BarsCopy)
}

// SeedDaily installs a symbol's daily ATR / avg-volume context (startup REST seed).
func (e *Engine) SeedDaily(sym string, atr, avgVol float64) { e.store.SetDaily(sym, atr, avgVol) }

// SeedBars replays today's backfilled session bars into the store (no detection — live
// signals begin with the first streamed bar, so a restart can't re-fire old setups).
func (e *Engine) SeedBars(sym string, bars []Bar) {
	for _, b := range bars {
		e.store.OnBar(sym, time.Unix(b.Time, 0), b.Open, b.High, b.Low, b.Close, b.Volume)
	}
}

// OnBar is the live entry point for every streamed 1-minute bar (any symbol; non-universe
// symbols are ignored, context symbols only update the market backdrop).
func (e *Engine) OnBar(sym string, t time.Time, o, h, l, c, v float64) {
	isCtx := false
	for _, cs := range e.uni.ContextSymbols {
		if cs == sym {
			isCtx = true
			break
		}
	}
	if !isCtx && !e.uni.Has(sym) {
		return
	}
	bars := e.store.OnBar(sym, t, o, h, l, c, v)
	e.sweepOnNewDay()
	if bars == nil || isCtx {
		return
	}
	e.resolveOutcomes(sym, bars[len(bars)-1])
	e.detect(sym, bars)
}

// sweepOnNewDay drops stale cool/dayCnt entries once the store rolls to a new session
// day, so both maps stay bounded across a long-running process instead of growing
// forever: cool entries older than 24h, and dayCnt entries not from today.
func (e *Engine) sweepOnNewDay() {
	day := e.store.Day()
	if day == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if day == e.sweptDay {
		return
	}
	e.sweptDay = day
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for k, last := range e.cool {
		if last < cutoff {
			delete(e.cool, k)
		}
	}
	for k := range e.dayCnt {
		if idx := strings.LastIndex(k, "|"); idx < 0 || k[idx+1:] != day {
			delete(e.dayCnt, k)
		}
	}
}

// detect runs every strategy on the symbol's latest completed bar.
func (e *Engine) detect(sym string, bars []Bar) {
	last := bars[len(bars)-1]
	atr, avgVol := e.store.Daily(sym)
	if atr <= 0 || !tradable(last.Close, avgVol) {
		return
	}
	open := e.store.SessionOpen()
	if open == 0 {
		return
	}
	mktOK, mktPct := e.store.Market()
	ctx := Context{
		SessionOpen: open,
		ATR:         atr,
		AvgVolume:   avgVol,
		RVOL:        e.store.RVOL(sym, last.Time),
		MarketOK:    mktOK,
		MarketPct:   mktPct,
	}
	e.mu.Lock()
	extra := e.ExtraFeatures
	e.mu.Unlock()

	for _, st := range e.strats {
		sig := st.Detect(sym, bars, ctx)
		if sig == nil {
			continue
		}
		if extra != nil {
			if sig.Features == nil {
				sig.Features = map[string]float64{}
			}
			for k, v := range extra(sym) {
				if _, exists := sig.Features[k]; !exists {
					sig.Features[k] = v
				}
			}
		}
		e.publish(*sig)
	}
}

// publish applies cooldowns, stamps identity, logs (with the time-of-day verdict),
// registers the counterfactual, and hands the signal to the execution hook (if any).
func (e *Engine) publish(sig Signal) {
	day := time.Unix(sig.Time, 0).In(e.et).Format("2006-01-02")
	key := sig.Strategy + "|" + sig.Symbol
	bucket := 0
	if open := e.store.SessionOpen(); open > 0 {
		bucket = minuteOf(sig.Time, open) / 30
	}

	e.mu.Lock()
	if last, ok := e.cool[key]; ok && sig.Time-last < int64(signalCooldown.Seconds()) {
		e.mu.Unlock()
		return
	}
	if e.dayCnt[key+"|"+day] >= maxPerDay {
		e.mu.Unlock()
		return
	}
	e.cool[key] = sig.Time
	e.dayCnt[key+"|"+day]++
	e.seq++
	sig.ID = fmt.Sprintf("%s-%s-%d-%d", sig.Strategy, sig.Symbol, sig.Time, e.seq)
	sig.Sector = e.uni.Sector(sig.Symbol)
	todBlocked := e.todBlocked(sig.Strategy, bucket)
	e.pending = append(e.pending, &pendingOutcome{sig: sig, todBucket: bucket})
	onSignal := e.OnSignal
	e.mu.Unlock()

	e.writeJSONL(day, map[string]interface{}{
		"type": "signal", "signal": sig, "tod_bucket": bucket, "tod_blocked": todBlocked,
	})
	verdict := ""
	if todBlocked {
		verdict = " [TOD-blocked]"
	}
	log.Printf("[signals] %s %s @ $%.2f (stop %.2f / target %.2f, rvol %.1f, q %.1f)%s",
		sig.Strategy, sig.Symbol, sig.Price, sig.Suggested.Stop, sig.Suggested.Target, sig.Features["rvol"], sig.Quality, verdict)
	if onSignal != nil {
		onSignal(sig)
	}
}

// resolveOutcomes advances the counterfactual brackets of this symbol's pending signals
// using the newly completed bar. First-touch; a bar that spans both levels counts as a
// STOP (conservative). Unresolved brackets close at the 15:55 bar (EOD flatten).
func (e *Engine) resolveOutcomes(sym string, b Bar) {
	open := e.store.SessionOpen()
	e.mu.Lock()
	var due []map[string]interface{}
	kept := e.pending[:0]
	for _, p := range e.pending {
		if p.done {
			continue
		}
		if p.sig.Symbol != sym || b.Time <= p.sig.Time {
			kept = append(kept, p)
			continue
		}
		exit, reason := 0.0, ""
		switch {
		case b.Low <= p.sig.Suggested.Stop:
			exit, reason = p.sig.Suggested.Stop, "stop"
		case b.High >= p.sig.Suggested.Target:
			exit, reason = p.sig.Suggested.Target, "target"
		case p.sig.MaxHoldMin > 0 && (b.Time-p.sig.Time)/60 >= int64(p.sig.MaxHoldMin):
			exit, reason = b.Close, "time"
		case open > 0 && minuteOf(b.Time, open) >= eodFlattenMin:
			exit, reason = b.Close, "eod"
		}
		if reason == "" {
			kept = append(kept, p)
			continue
		}
		p.done = true
		risk := p.sig.Suggested.Entry - p.sig.Suggested.Stop
		r := 0.0
		if risk > 0 {
			r = (exit - p.sig.Suggested.Entry) / risk
		}
		// Grow the persisted time-of-day stats with this resolved outcome.
		tk := p.sig.Strategy + "|" + strconv.Itoa(p.todBucket)
		if e.tod[tk] == nil {
			e.tod[tk] = &todStat{}
		}
		e.tod[tk].N++
		e.tod[tk].Sum += r
		e.saveTOD()
		due = append(due, map[string]interface{}{
			"type":          "outcome",
			"id":            p.sig.ID,
			"strategy":      p.sig.Strategy,
			"symbol":        p.sig.Symbol,
			"signal_time":   p.sig.Time,
			"exit_time":     b.Time,
			"exit_price":    exit,
			"exit_reason":   reason,
			"r_multiple":    r,
			"pnl_per_share": exit - p.sig.Suggested.Entry,
			"minutes_held":  (b.Time - p.sig.Time) / 60,
		})
	}
	e.pending = kept
	e.mu.Unlock()

	for _, rec := range due {
		day := time.Unix(b.Time, 0).In(e.et).Format("2006-01-02")
		e.writeJSONL(day, rec)
	}
}

// writeJSONL appends one record to data/signals/<day>.jsonl. Best-effort — a logging
// failure never affects anything else.
func (e *Engine) writeJSONL(day string, rec map[string]interface{}) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(e.logDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(e.logDir, day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
