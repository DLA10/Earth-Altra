package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"live-optimus/backend/internal/risk"
	"live-optimus/backend/internal/signals"
)

// SignalTrader is the execution bridge that ships the validated signal-engine
// configuration (all six strategies) to the quant PAPER account. Decision chain for
// every published signal:
//
//	signal → TOD gate (only if QUANT_TOD_GATE; shadow-journaled otherwise) →
//	session/posture guards → daily loss cap → allocator slot/budget →
//	LLM entry judge (red-flag veto + conviction sizing) →
//	Manager (market entry → trailing-stop floor → Agent 3 exit loop → EOD flatten)
//
// Every decision, including every skip, is written to the quant decision log. The dip
// pipeline (dipwatch → Telegram → OnDip) is untouched and shares the same allocator, so
// the two can never oversubscribe the budget. Paper-only by construction: the broker
// holds paper keys; there is no code path from here to the live account.
type SignalTrader struct {
	eng     *Engine         // quant engine (allocator, decision log, candle prices, universe posture)
	mgr     *Manager        // position lifecycle (entry, stop floor, Agent 3, flatten)
	sigs    *signals.Engine // the live signal engine (TOD gate lives here)
	judge   *SignalJudge    // LLM red-flag reviewer (nil-safe: skipped when disabled)
	day     *risk.Day       // daily loss-cap tracker (approximate, halt-only)
	appCtx  context.Context
	loc     *time.Location
	enabled bool
	todGate bool // enforce the TOD gate (false = shadow-only: journaled, never blocks)

	// Demoted, when set, reports whether the eval scoreboard has benched a strategy
	// (negative rolling expectancy or CUSUM alarm) — benched strategies keep journaling
	// but may not trade.
	Demoted func(strategy string) bool
}

// NewSignalTrader wires the bridge. limits supplies the daily loss cap; the allocator
// (inside eng) owns budget/slots/sizing. todGate enforces the time-of-day gate; when
// false the gate runs shadow-only (the engine still journals every bucket verdict).
func NewSignalTrader(ctx context.Context, eng *Engine, mgr *Manager, sigs *signals.Engine, judge *SignalJudge, limits risk.Limits, enabled, todGate bool) *SignalTrader {
	t := &SignalTrader{
		eng: eng, mgr: mgr, sigs: sigs, judge: judge,
		day: risk.NewDay(limits, eng.loc), appCtx: ctx, loc: eng.loc, enabled: enabled, todGate: todGate,
	}
	// Feed the loss-cap tracker with approximate realized P&L from every close.
	mgr.OnClosed = func(sym string, pnl float64) {
		t.day.OnRealized(pnl, time.Now())
		if realized, halted := t.day.Realized(time.Now()); halted {
			log.Printf("[signal-trader] DAILY LOSS CAP HIT (day P&L ≈ $%.2f) — no more entries today", realized)
			t.eng.logRec(LogRecord{Agent: "signal_trader", Event: "skip",
				Note: fmt.Sprintf("daily loss cap tripped (approx day P&L $%.2f)", realized)})
		}
	}
	return t
}

// OnSignal is the signals.Engine hook. It must not block the market-data path, so the
// full decision chain runs on its own goroutine.
func (t *SignalTrader) OnSignal(sig signals.Signal) {
	if !t.enabled {
		return
	}
	go t.handle(sig)
}

func (t *SignalTrader) handle(sig signals.Signal) {
	sym := sig.Symbol
	skip := func(reason string) {
		t.eng.logRec(LogRecord{Agent: "signal_trader", Event: "skip", Symbol: sym,
			Note: sig.Strategy + ": " + reason})
		log.Printf("[signal-trader] skip %s %s — %s", sig.Strategy, sym, reason)
	}

	// 1) The learned time-of-day gate (decayed buckets). Enforcement is switchable
	// (QUANT_TOD_GATE): the 12-month re-test showed its edge is regime-dependent, so the
	// operator can demote it to shadow without losing the journal evidence.
	if t.todGate && !t.sigs.EntryAllowed(sig) {
		skip("time-of-day bucket has proven negative expectancy")
		return
	}
	// 1b) Scoreboard demotion (evals): benched strategies journal but don't trade.
	if t.Demoted != nil && t.Demoted(sig.Strategy) {
		skip("strategy demoted by the eval scoreboard")
		return
	}
	// 2) Session guard: nothing fresh after 15:30 ET (the manager flattens at 15:55).
	now := time.Now().In(t.loc)
	if now.Hour() > 15 || (now.Hour() == 15 && now.Minute() >= 30) {
		skip("too late in the session")
		return
	}
	// 3) Market posture (pre-market Strategist read, when present).
	if t.eng.universe != nil && t.eng.universe.StandDown() {
		skip("regime stand_down")
		return
	}
	// 4) Daily loss cap.
	if err := t.day.CanEnter(t.eng.alloc.OpenCount(), time.Now()); err != nil {
		skip(err.Error())
		return
	}
	// 5) Slot/budget/duplicate check (cheap, before spending an LLM call).
	if !t.eng.alloc.CanFund(sym) {
		skip("no slot/capital free (or already held)")
		return
	}

	// 6) LLM entry judge: red-flag veto + conviction (drives full/half sizing).
	conf := 0.6 // judge-disabled fallback: half-slice conviction
	if t.judge.Enabled() {
		snap := t.judgeSnapshot(sig)
		ctx, cancel := context.WithTimeout(t.appCtx, 25*time.Second)
		dec, usage, err := t.judge.Decide(ctx, snap)
		cancel()
		if err != nil {
			skip("judge error (fail-closed): " + err.Error())
			return
		}
		t.eng.logRec(LogRecord{Agent: "signal_judge", Event: "decision", Symbol: sym, Model: t.judge.model,
			Input: json.RawMessage(snap), Output: dec, Tokens: &usage})
		log.Printf("[signal-judge] %s %s -> %s (%.2f): %s", sig.Strategy, sym, dec.Action, dec.Confidence, dec.Reason)
		if !dec.IsBuy() {
			return // veto — already logged as the judge's decision
		}
		conf = dec.Confidence
	}
	// Cautious posture (Strategist): demand higher conviction before committing.
	if t.eng.universe != nil && strings.EqualFold(t.eng.universe.Regime().Posture, "cautious") && conf < 0.65 {
		skip(fmt.Sprintf("cautious posture requires conviction ≥ 0.65 (got %.2f)", conf))
		return
	}

	// 7) Deterministic sizing + funding (conviction picks full vs half slice).
	size := t.eng.alloc.Size(conf)
	if size <= 0 {
		skip(fmt.Sprintf("allocator returned no size (conf %.2f)", conf))
		return
	}
	if sig.Price <= 0 || math.Floor(size/sig.Price) < 1 {
		skip(fmt.Sprintf("$%.0f can't buy 1 share at $%.2f", size, sig.Price))
		return
	}
	if !t.eng.alloc.Fund(sym, size) {
		skip("slot taken while deciding")
		return
	}
	t.eng.logRec(LogRecord{Agent: "signal_trader", Event: "order", Symbol: sym,
		Note: fmt.Sprintf("%s: funded $%.0f (conf %.2f)", sig.Strategy, size, conf)})
	log.Printf("[signal-trader] ENTER %s %s $%.0f (conf %.2f)", sig.Strategy, sym, size, conf)

	// 8) Hand off to the manager: entry → stop floor → Agent 3 → flatten. Blocking is
	// fine — we're already on a dedicated goroutine.
	t.mgr.OpenPosition(t.appCtx, sym, conf, size)
}

// judgeSnapshot assembles the judge's decision context from the signal.
func (t *SignalTrader) judgeSnapshot(sig signals.Signal) string {
	riskPct := 0.0
	rr := 0.0
	if sig.Suggested.Entry > 0 && sig.Suggested.Entry > sig.Suggested.Stop {
		riskAbs := sig.Suggested.Entry - sig.Suggested.Stop
		riskPct = riskAbs / sig.Suggested.Entry * 100
		rr = (sig.Suggested.Target - sig.Suggested.Entry) / riskAbs
	}
	posture := "normal"
	if t.eng.universe != nil {
		if p := t.eng.universe.Regime().Posture; p != "" {
			posture = p
		}
	}
	snap := map[string]interface{}{
		"signal_id": sig.ID, // joins the judge's decision to its counterfactual outcome (evals)
		"strategy":  sig.Strategy,
		"symbol":    sig.Symbol,
		"sector":    sig.Sector,
		"now_et":    time.Now().In(t.loc).Format("15:04"),
		"price":     sig.Price,
		"bracket": map[string]interface{}{
			"entry": sig.Suggested.Entry, "stop": sig.Suggested.Stop, "target": sig.Suggested.Target,
			"risk_pct": round2(riskPct), "reward_risk": round2(rr),
		},
		"features": sig.Features,
		"posture":  posture,
	}
	b, _ := json.Marshal(snap)
	return string(b)
}
