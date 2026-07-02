package signals

import "math"

// DefaultStrategies returns the v1 strategy set (all long-only, intraday). Order matters
// only for log readability. Each detector is deliberately simple and well-understood —
// the edge comes from selectivity + risk control + the ML gate later, not cleverness.
// ALL five run in shadow (their signals + outcomes are the ML dataset); only
// PromotedStrategies may ever place paper orders.
func DefaultStrategies() []Strategy {
	return []Strategy{
		ORBBreakout{},
		VWAPReclaim{},
		MomentumContinuation{},
		DipBounce{},
		RelativeStrength{},
	}
}

// PromotedStrategies is the evidence-gated EXECUTABLE set — the only strategies allowed
// to drive paper orders when signal execution is enabled. Membership is earned via
// walk-forward backtest + shadow evidence and revoked the same way (QUANT_VISION §5).
//
// Current evidence (3-month SIP replay, 2026-03-31 → 2026-07-01, cooldowns matching the
// live engine; run `go run ./cmd/backtest -days 63 -strategies vwap_reclaim,momentum_cont`):
//   - vwap_reclaim   +$1,180 full window, +$272 in the adverse June regime → promoted
//   - momentum_cont    +$61 full window,  +$70 in June → promoted (probation: low margin)
//   - dip_bounce / orb_breakout / rel_strength: profitable Apr–May, negative June →
//     regime-dependent; shadow-only until the ML gate can condition them on regime.
func PromotedStrategies() []Strategy {
	return []Strategy{
		VWAPReclaim{},
		MomentumContinuation{},
	}
}

// newSignal assembles the common Signal fields. Quality is a deterministic rank hint:
// participation (RVOL) dominates, a friendly market adds a nudge.
func newSignal(strategy, sym string, last Bar, entry, stop, target float64, ctx Context, feats map[string]float64) *Signal {
	feats["rvol"] = ctx.RVOL
	feats["atr"] = ctx.ATR
	feats["market_pct"] = ctx.MarketPct
	if ctx.MarketOK {
		feats["market_ok"] = 1
	} else {
		feats["market_ok"] = 0
	}
	feats["minute"] = float64(minuteOf(last.Time, ctx.SessionOpen))
	q := clampF(ctx.RVOL, 0, 3)
	if ctx.MarketOK {
		q += 0.5
	}
	return &Signal{
		Strategy:  strategy,
		Symbol:    sym,
		Time:      last.Time,
		Price:     entry,
		Suggested: Suggested{Entry: entry, Stop: stop, Target: target},
		Quality:   q,
		Features:  feats,
	}
}

// riskOK sanity-checks a bracket: stop below entry, risk neither microscopic (noise)
// nor larger than half a daily ATR (a broken setup, not a trade).
func riskOK(entry, stop, atr float64) bool {
	risk := entry - stop
	return risk > 0 && risk >= 0.0015*entry && risk <= 0.55*atr
}

// ---- 1. Opening-range breakout ----------------------------------------------------

// ORBBreakout goes long when price breaks the 15-minute opening-range high on elevated
// participation with the market behind it. Classic momentum; mornings only.
type ORBBreakout struct{}

func (ORBBreakout) Name() string { return "orb_breakout" }

func (ORBBreakout) Detect(sym string, bars []Bar, ctx Context) *Signal {
	n := len(bars)
	if n < 18 || ctx.ATR <= 0 {
		return nil
	}
	last, prev := bars[n-1], bars[n-2]
	minute := minuteOf(last.Time, ctx.SessionOpen)
	if minute < 16 || minute > 150 { // 09:46–12:00 ET — ORB is a morning trade
		return nil
	}
	// Opening range = the first 15 minutes.
	orHigh, orLow := math.Inf(-1), math.Inf(1)
	seen := 0
	for _, b := range bars {
		if minuteOf(b.Time, ctx.SessionOpen) >= 15 {
			break
		}
		if b.High > orHigh {
			orHigh = b.High
		}
		if b.Low < orLow {
			orLow = b.Low
		}
		seen++
	}
	if seen < 10 || math.IsInf(orHigh, -1) {
		return nil // session data doesn't cover the open — can't anchor the range
	}
	// The cross: this bar closes above the OR high, the previous one didn't.
	if !(prev.Close <= orHigh && last.Close > orHigh) {
		return nil
	}
	if ctx.RVOL < 1.5 || !ctx.MarketOK {
		return nil
	}
	// Volume confirmation on the breakout bar itself.
	if av := avgBarVolume(bars); av <= 0 || last.Volume < 1.2*av {
		return nil
	}
	entry := last.Close
	stop := entry - 0.30*ctx.ATR
	target := entry + 0.45*ctx.ATR
	if !riskOK(entry, stop, ctx.ATR) {
		return nil
	}
	return newSignal("orb_breakout", sym, last, entry, stop, target, ctx, map[string]float64{
		"or_high":       orHigh,
		"or_low":        orLow,
		"or_range_atr":  (orHigh - orLow) / ctx.ATR,
		"break_ext_atr": (last.Close - orHigh) / ctx.ATR,
		"bar_vol_ratio": last.Volume / math.Max(avgBarVolume(bars), 1),
	})
}

// ---- 2. VWAP reclaim ----------------------------------------------------------------

// VWAPReclaim is mean reversion: price flushes meaningfully below VWAP, then closes back
// above it on a green bar with momentum recovering — the generalized dip-buy.
type VWAPReclaim struct{}

func (VWAPReclaim) Name() string { return "vwap_reclaim" }

func (VWAPReclaim) Detect(sym string, bars []Bar, ctx Context) *Signal {
	n := len(bars)
	if n < 25 || ctx.ATR <= 0 {
		return nil
	}
	last, prev := bars[n-1], bars[n-2]
	minute := minuteOf(last.Time, ctx.SessionOpen)
	if minute < 20 || minute > 330 { // 09:50–15:00 ET
		return nil
	}
	vw := vwapSeries(bars)
	// The cross: previous bar closed below its VWAP, this one closes above.
	if !(prev.Close < vw[n-2] && last.Close > vw[n-1]) || last.Close <= last.Open {
		return nil
	}
	// A real flush preceded it: within the last 30 bars price was ≥ 0.15 ATR under VWAP.
	flush := 0.0
	lo := n - 30
	if lo < 0 {
		lo = 0
	}
	for i := lo; i < n-1; i++ {
		if d := vw[i] - bars[i].Close; d > flush {
			flush = d
		}
	}
	if flush < 0.15*ctx.ATR {
		return nil
	}
	if ctx.RVOL < 1.2 {
		return nil
	}
	r := rsiWilder(closesOf(bars), 14)
	if r < 35 || r > 70 { // recovering from oversold, not an overbought chase
		return nil
	}
	// Stop under the reclaim structure: the lowest low of the last 10 bars.
	swingLow := math.Inf(1)
	for i := n - 10; i < n; i++ {
		if bars[i].Low < swingLow {
			swingLow = bars[i].Low
		}
	}
	entry := last.Close
	stop := swingLow - 0.05*ctx.ATR
	if !riskOK(entry, stop, ctx.ATR) {
		return nil
	}
	target := entry + 1.3*(entry-stop)
	return newSignal("vwap_reclaim", sym, last, entry, stop, target, ctx, map[string]float64{
		"flush_atr":     flush / ctx.ATR,
		"rsi":           r,
		"vwap_dist_pct": (last.Close - vw[n-1]) / vw[n-1] * 100,
		"swing_low":     swingLow,
	})
}

// ---- 3. Momentum continuation ---------------------------------------------------------

// MomentumContinuation goes long the break to a new session high after a shallow, orderly
// pullback in an established uptrend (price above a rising VWAP), market confirming.
// The bracket is parameterized (in ATRs) so the backtester can sweep it; zero values fall
// back to the defaults. MaxHoldMin (0 = none) time-boxes the trade.
type MomentumContinuation struct {
	StopATR    float64 // default 0.35
	TargetATR  float64 // default 0.50
	MaxHoldMin int     // default 0 (hold to EOD)
}

func (MomentumContinuation) Name() string { return "momentum_cont" }

func (m MomentumContinuation) params() (stop, target float64) {
	stop, target = m.StopATR, m.TargetATR
	if stop <= 0 {
		stop = 0.35
	}
	if target <= 0 {
		target = 0.50
	}
	return
}

func (m MomentumContinuation) Detect(sym string, bars []Bar, ctx Context) *Signal {
	n := len(bars)
	if n < 40 || ctx.ATR <= 0 {
		return nil
	}
	last, prev := bars[n-1], bars[n-2]
	minute := minuteOf(last.Time, ctx.SessionOpen)
	if minute < 30 || minute > 330 {
		return nil
	}
	vw := vwapSeries(bars)
	if last.Close <= vw[n-1] || vw[n-1] <= vw[n-31] { // above a RISING vwap
		return nil
	}
	// Prior high excluding the last 3 bars; this bar must break it, the previous not.
	priorHigh := math.Inf(-1)
	for i := 0; i < n-3; i++ {
		if bars[i].High > priorHigh {
			priorHigh = bars[i].High
		}
	}
	if !(prev.Close <= priorHigh && last.Close > priorHigh) {
		return nil
	}
	// An orderly pullback preceded the break: within the last 45 bars the low dipped
	// 0.12–0.40 ATR below that prior high (shallow — deeper means the trend broke).
	lo := n - 45
	if lo < 0 {
		lo = 0
	}
	pull := 0.0
	for i := lo; i < n-1; i++ {
		if d := priorHigh - bars[i].Low; d > pull {
			pull = d
		}
	}
	if pull < 0.12*ctx.ATR || pull > 0.40*ctx.ATR {
		return nil
	}
	if ctx.RVOL < 1.3 || !ctx.MarketOK {
		return nil
	}
	stopATR, targetATR := m.params()
	entry := last.Close
	stop := entry - stopATR*ctx.ATR
	target := entry + targetATR*ctx.ATR
	if !riskOK(entry, stop, ctx.ATR) {
		return nil
	}
	sig := newSignal("momentum_cont", sym, last, entry, stop, target, ctx, map[string]float64{
		"pullback_atr":  pull / ctx.ATR,
		"vwap_dist_pct": (last.Close - vw[n-1]) / vw[n-1] * 100,
		"prior_high":    priorHigh,
	})
	sig.MaxHoldMin = m.MaxHoldMin
	return sig
}

// ---- 4. Dip bounce -----------------------------------------------------------------

// DipBounce is the ported dip-watcher recipe on session bars: a meaningful pullback off
// the day high into oversold, below VWAP, then a confirmed multi-bar bounce off the low.
type DipBounce struct{}

func (DipBounce) Name() string { return "dip_bounce" }

func (DipBounce) Detect(sym string, bars []Bar, ctx Context) *Signal {
	n := len(bars)
	if n < 35 || ctx.ATR <= 0 {
		return nil
	}
	last := bars[n-1]
	minute := minuteOf(last.Time, ctx.SessionOpen)
	if minute < 30 || minute > 330 {
		return nil
	}
	dayHigh, _, hiIdx := sessionHighLow(bars)
	// The dip: the lowest low AFTER the day high.
	dipLow := math.Inf(1)
	for i := hiIdx; i < n; i++ {
		if bars[i].Low < dipLow {
			dipLow = bars[i].Low
		}
	}
	depth := dayHigh - dipLow
	if depth < 0.5*ctx.ATR { // not a meaningful pullback
		return nil
	}
	vw := vwapSeries(bars)
	if last.Close >= vw[n-1] { // still below VWAP — we're buying the bounce, not the recovery
		return nil
	}
	closes := closesOf(bars)
	rNow := rsiWilder(closes, 14)
	// Was genuinely oversold within the last 15 bars, and is now turning up.
	rMin := 100.0
	for i := n - 15; i < n; i++ {
		if r := rsiWilder(closes[:i+1], 14); r < rMin {
			rMin = r
		}
	}
	if rMin > 35 || rNow > 48 {
		return nil
	}
	// Confirmed bounce: green bar, net progress over the last 5 bars, clear of the low.
	if last.Close <= last.Open || n < 6 || last.Close <= bars[n-6].Close || last.Close < dipLow+0.10*ctx.ATR {
		return nil
	}
	if ctx.RVOL < 1.3 {
		return nil
	}
	entry := last.Close
	stop := dipLow - 0.10*ctx.ATR
	if !riskOK(entry, stop, ctx.ATR) {
		return nil
	}
	target := entry + 1.2*(entry-stop)
	return newSignal("dip_bounce", sym, last, entry, stop, target, ctx, map[string]float64{
		"depth_atr":     depth / ctx.ATR,
		"rsi":           rNow,
		"rsi_min15":     rMin,
		"dip_low":       dipLow,
		"day_high":      dayHigh,
		"vwap_dist_pct": (last.Close - vw[n-1]) / vw[n-1] * 100,
	})
}

// ---- 5. Relative strength -----------------------------------------------------------

// RelativeStrength buys a stock making new session highs above VWAP while the broad
// market (QQQ) is flat or red — cross-sectional leadership that tends to accelerate when
// the tide turns back up.
type RelativeStrength struct{}

func (RelativeStrength) Name() string { return "rel_strength" }

func (RelativeStrength) Detect(sym string, bars []Bar, ctx Context) *Signal {
	n := len(bars)
	if n < 30 || ctx.ATR <= 0 {
		return nil
	}
	last, prev := bars[n-1], bars[n-2]
	minute := minuteOf(last.Time, ctx.SessionOpen)
	if minute < 30 || minute > 330 {
		return nil
	}
	if ctx.MarketPct > 0.05 { // only interesting when the tide ISN'T helping
		return nil
	}
	open := bars[0].Open
	if open <= 0 {
		return nil
	}
	symPct := (last.Close - open) / open * 100
	atrPct := ctx.ATR / last.Close * 100
	if symPct < math.Max(0.5, 0.35*atrPct) { // meaningfully green vs its own volatility
		return nil
	}
	vw := vwapSeries(bars)
	if last.Close <= vw[n-1] {
		return nil
	}
	// Fresh strength: breaking to a new session high right now.
	priorHigh := math.Inf(-1)
	for i := 0; i < n-1; i++ {
		if bars[i].High > priorHigh {
			priorHigh = bars[i].High
		}
	}
	if !(prev.Close <= priorHigh && last.Close > priorHigh) {
		return nil
	}
	if ctx.RVOL < 1.5 {
		return nil
	}
	entry := last.Close
	stop := entry - 0.30*ctx.ATR
	target := entry + 0.45*ctx.ATR
	if !riskOK(entry, stop, ctx.ATR) {
		return nil
	}
	return newSignal("rel_strength", sym, last, entry, stop, target, ctx, map[string]float64{
		"sym_pct_open": symPct,
		"rs_spread":    symPct - ctx.MarketPct,
		"atr_pct":      atrPct,
	})
}
