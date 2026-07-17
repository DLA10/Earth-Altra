// Package signals is the multi-strategy intraday signal engine (QUANT_VISION.md Phase 1).
// Deterministic strategy detectors scan the curated quant universe (QUANT_UNIVERSE.json)
// on completed 1-minute bars and publish typed Signal events with a full feature snapshot
// and ATR-scaled bracket suggestions. Every signal is logged together with its
// counterfactual outcome (would the bracket have won?), traded or not — the running
// system generates its own labeled ML training dataset. The same detectors power both
// the live engine (engine.go, shadow-first) and the backtester (backtest.go).
//
// Paper-only by design: this package never touches the live account or the live order
// path. It reads market data and writes JSONL logs; execution (when enabled) goes through
// the quant paper broker exclusively.
package signals

import (
	"math"
)

// Bar is one 1-minute OHLCV bar (unix-second open time). The package keeps its own bar
// type so detectors and the backtester are standalone (no dependency on the live engine).
type Bar struct {
	Time   int64   `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

// Suggested is the deterministic, ATR-scaled bracket a signal proposes. Entry is the
// signal-bar close; Stop/Target are what the counterfactual outcome (and the backtester)
// measure against.
type Suggested struct {
	Entry  float64 `json:"entry"`
	Stop   float64 `json:"stop"`
	Target float64 `json:"target"`
}

// Signal is one detected setup. Features carries the full numeric snapshot at trigger
// time — this is the future ML training row, so detectors should be generous here.
type Signal struct {
	ID        string             `json:"id"`
	Strategy  string             `json:"strategy"`
	Symbol    string             `json:"symbol"`
	Sector    string             `json:"sector,omitempty"`
	Time      int64              `json:"time"` // unix seconds (signal bar close time)
	Price     float64            `json:"price"`
	Suggested Suggested          `json:"suggested"`
	// MaxHoldMin closes the position at market after this many minutes if neither
	// bracket level has been hit (0 = hold until the EOD flatten). Tames strategies
	// whose edge decays with holding time (momentum_cont's backtests showed most of its
	// losses came from stale positions drifting into the close).
	MaxHoldMin int                `json:"max_hold_min,omitempty"`
	Quality    float64            `json:"quality"` // deterministic pre-score (rank hint)
	Features   map[string]float64 `json:"features"`

	// Trend-alignment verdict, stamped at publish (see alignment.go). TrendCell is the
	// (market, stock) rolling-trend cell, e.g. "mkt-down/sym-down"; AlignOK is whether
	// the strategy's playbook permits trading that cell. nil = trends unknown at publish
	// (fail-open). Journaled on both the signal and its counterfactual outcome so the
	// eval scoreboard can judge each strategy on its playbook cells only.
	TrendCell string `json:"trend_cell,omitempty"`
	AlignOK   *bool  `json:"align_ok,omitempty"`
}

// Context is everything a detector needs beyond the symbol's own session bars.
type Context struct {
	SessionOpen int64   // 09:30 ET unix for the session the bars belong to
	ATR         float64 // daily ATR(14) — the volatility yardstick for stops/targets
	AvgVolume   float64 // 20-day average daily volume
	RVOL        float64 // today's cumulative volume vs the time-adjusted expectation
	MarketOK    bool    // SPY/QQQ backdrop is not risk-off
	MarketPct   float64 // QQQ % from its open (the tide, signed)
}

// Strategy is one deterministic detector. Detect is called after each COMPLETED 1-minute
// bar with the symbol's regular-session bars so far (oldest first). Return nil for no
// signal. Detectors must be pure (no state) so live and backtest behave identically.
type Strategy interface {
	Name() string
	Detect(sym string, bars []Bar, ctx Context) *Signal
}

// ---- universal filters (applied by the engine/backtester before detectors run) ----

const (
	minPrice     = 5.0
	maxPrice     = 1000.0
	minAvgVolume = 1_000_000
	// regularSessionMin is the length of the regular session in minutes.
	regularSessionMin = 390
	// eodFlattenMin is the session minute (15:55 ET) at which open counterfactual
	// brackets and backtest positions are closed at market.
	eodFlattenMin = 385
)

// tradable reports whether a symbol currently passes the universal liquidity/price gate.
func tradable(price, avgVolume float64) bool {
	return price >= minPrice && price <= maxPrice && avgVolume >= minAvgVolume
}

// ---- shared indicator math (pure, reused by every detector) ----

// closesOf extracts closing prices.
func closesOf(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// rsiWilder computes Wilder's RSI(length) over closes; returns 50 when data is short.
func rsiWilder(closes []float64, length int) float64 {
	if len(closes) <= length {
		return 50
	}
	var gain, loss float64
	for i := 1; i <= length; i++ {
		d := closes[i] - closes[i-1]
		if d >= 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	ag, al := gain/float64(length), loss/float64(length)
	for i := length + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
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

// vwapSeries returns the cumulative session VWAP at each bar (typical-price weighted).
func vwapSeries(bars []Bar) []float64 {
	out := make([]float64, len(bars))
	var pv, vol float64
	for i, b := range bars {
		tp := (b.High + b.Low + b.Close) / 3
		pv += tp * b.Volume
		vol += b.Volume
		if vol > 0 {
			out[i] = pv / vol
		} else {
			out[i] = b.Close
		}
	}
	return out
}

// sessionHighLow returns the session high/low and the index of the high bar.
func sessionHighLow(bars []Bar) (high, low float64, highIdx int) {
	high, low = math.Inf(-1), math.Inf(1)
	for i, b := range bars {
		if b.High > high {
			high = b.High
			highIdx = i
		}
		if b.Low < low {
			low = b.Low
		}
	}
	return
}

// avgBarVolume returns the mean per-bar volume of the session so far.
func avgBarVolume(bars []Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	var sum float64
	for _, b := range bars {
		sum += b.Volume
	}
	return sum / float64(len(bars))
}

// minuteOf returns the session minute of a bar (0 = the 09:30 bar).
func minuteOf(barTime, sessionOpen int64) int {
	return int((barTime - sessionOpen) / 60)
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
