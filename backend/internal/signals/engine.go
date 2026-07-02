package signals

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

	// OnSignal, when set, receives every published signal (for paper execution later).
	OnSignal func(Signal)

	mu      sync.Mutex
	cool    map[string]int64 // "strategy|symbol" -> last signal unix (30-min cooldown)
	dayCnt  map[string]int   // "strategy|symbol|day" -> signals published today
	pending []*pendingOutcome
	seq     int64
}

// pendingOutcome tracks one published signal until its counterfactual bracket resolves.
type pendingOutcome struct {
	sig  Signal
	done bool
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
	return &Engine{
		store:  NewStore(),
		uni:    uni,
		strats: DefaultStrategies(),
		logDir: filepath.Join(dataDir, "signals"),
		et:     loc,
		cool:   map[string]int64{},
		dayCnt: map[string]int{},
	}
}

// Universe exposes the loaded universe (for stream subscription wiring).
func (e *Engine) Universe() *Universe { return e.uni }

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
	if bars == nil || isCtx {
		return
	}
	e.resolveOutcomes(sym, bars[len(bars)-1])
	e.detect(sym, bars)
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
	for _, st := range e.strats {
		sig := st.Detect(sym, bars, ctx)
		if sig == nil {
			continue
		}
		e.publish(*sig)
	}
}

// publish applies cooldowns, stamps identity, logs, registers the counterfactual, and
// hands the signal to the execution hook (if any).
func (e *Engine) publish(sig Signal) {
	day := time.Unix(sig.Time, 0).In(e.et).Format("2006-01-02")
	key := sig.Strategy + "|" + sig.Symbol

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
	e.pending = append(e.pending, &pendingOutcome{sig: sig})
	e.mu.Unlock()

	e.writeJSONL(day, map[string]interface{}{"type": "signal", "signal": sig})
	log.Printf("[signals] %s %s @ $%.2f (stop %.2f / target %.2f, rvol %.1f, q %.1f)",
		sig.Strategy, sig.Symbol, sig.Price, sig.Suggested.Stop, sig.Suggested.Target, sig.Features["rvol"], sig.Quality)
	if e.OnSignal != nil {
		e.OnSignal(sig)
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
