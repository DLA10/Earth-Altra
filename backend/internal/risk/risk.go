// Package risk holds the deterministic guardrails every automated (paper) strategy
// trades under. Pure rules, no models, no LLM — the whole point is that these hold even
// when everything upstream is wrong. Shared by the backtester and (when signal execution
// is enabled) the live-paper pipeline. Never used by, or wired into, the real-money path.
package risk

import (
	"fmt"
	"sync"
	"time"
)

// Limits are the hard caps. Zero values mean "no limit" only for OvernightCapUSD; the
// others should always be set.
type Limits struct {
	DailyLossCapUSD    float64 // realized day P&L at/below -cap => halt entries, flatten
	MaxRiskPerTradeUSD float64 // (entry - stop) × qty ceiling per trade
	MaxPositionUSD     float64 // per-position notional ceiling (also the sizing target)
	MaxConcurrent      int     // open positions ceiling
	OvernightCapUSD    float64 // total value allowed to hold past the close (0 = flatten all)
}

// Defaults returns the Phase-1 guardrails for the $8k paper account: three $2k slots,
// $40 max risk per trade, a $150 daily loss cap, and no overnight holds unless the
// operator raises the cap.
func Defaults() Limits {
	return Limits{
		DailyLossCapUSD:    150,
		MaxRiskPerTradeUSD: 40,
		MaxPositionUSD:     2000,
		MaxConcurrent:      3,
		OvernightCapUSD:    0,
	}
}

// Size returns the whole-share quantity for a bracket under these limits: the smaller of
// the notional target and the per-trade risk budget. 0 means "don't take the trade".
func (l Limits) Size(entry, stop float64) float64 {
	if entry <= 0 || stop <= 0 || stop >= entry {
		return 0
	}
	byNotional := l.MaxPositionUSD / entry
	byRisk := l.MaxRiskPerTradeUSD / (entry - stop)
	q := byNotional
	if byRisk < q {
		q = byRisk
	}
	return float64(int(q)) // floor to whole shares
}

// Day tracks one trading day's realized P&L and the halt state. Thread-safe.
type Day struct {
	mu       sync.Mutex
	limits   Limits
	day      string
	loc      *time.Location
	realized float64
	halted   bool
}

// NewDay creates a tracker (loc nil = UTC).
func NewDay(l Limits, loc *time.Location) *Day {
	if loc == nil {
		loc = time.UTC
	}
	return &Day{limits: l, loc: loc}
}

func (d *Day) roll(now time.Time) {
	if day := now.In(d.loc).Format("2006-01-02"); day != d.day {
		d.day = day
		d.realized = 0
		d.halted = false
	}
}

// OnRealized records a closed trade's P&L and trips the halt at the daily loss cap.
func (d *Day) OnRealized(pnl float64, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roll(now)
	d.realized += pnl
	if d.limits.DailyLossCapUSD > 0 && d.realized <= -d.limits.DailyLossCapUSD {
		d.halted = true
	}
}

// CanEnter reports whether a new position may be opened right now.
func (d *Day) CanEnter(openCount int, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roll(now)
	if d.halted {
		return fmt.Errorf("daily loss cap $%.0f hit (day P&L $%.2f) — no more entries today", d.limits.DailyLossCapUSD, d.realized)
	}
	if d.limits.MaxConcurrent > 0 && openCount >= d.limits.MaxConcurrent {
		return fmt.Errorf("max %d concurrent positions", d.limits.MaxConcurrent)
	}
	return nil
}

// Realized returns the tracked day P&L and halt state.
func (d *Day) Realized(now time.Time) (pnl float64, halted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roll(now)
	return d.realized, d.halted
}
