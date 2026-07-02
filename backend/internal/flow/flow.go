// Package flow estimates buyer- vs seller-initiated volume per symbol from the live
// trade and quote streams (Alpaca doesn't provide this directly). It uses the quote
// rule: a trade at/above the ask is buyer-initiated, at/below the bid seller-initiated,
// and one in between is assigned to the nearer side. It keeps both a day-cumulative
// tally and a rolling last-N-minutes window (the latter reacts to live pressure).
package flow

import (
	"sync"
	"time"
)

// rollWindowMin is the rolling window (minutes) for the "right now" pressure reading.
const rollWindowMin = 5

type bucket struct{ buy, sell float64 }

type symFlow struct {
	bid, ask        float64
	buyVol, sellVol float64           // day cumulative
	day             string            // ET date of the cumulative tally
	buckets         map[int64]*bucket // unix-minute -> volumes, for the rolling window
}

// Tracker holds per-symbol order-flow estimates.
type Tracker struct {
	mu sync.Mutex
	m  map[string]*symFlow
	et *time.Location
}

// New creates a Tracker.
func New() *Tracker {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return &Tracker{m: map[string]*symFlow{}, et: loc}
}

func (t *Tracker) get(sym string) *symFlow {
	f := t.m[sym]
	if f == nil {
		f = &symFlow{buckets: map[int64]*bucket{}}
		t.m[sym] = f
	}
	return f
}

// OnQuote records the latest bid/ask for classification.
func (t *Tracker) OnQuote(sym string, bid, ask float64) {
	if bid <= 0 || ask <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.get(sym)
	f.bid, f.ask = bid, ask
}

// OnTrade classifies a trade as buyer- or seller-initiated and accumulates its volume
// into both the day total and the current minute's rolling bucket.
func (t *Tracker) OnTrade(sym string, price, size float64, now time.Time) {
	if price <= 0 || size <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.get(sym)
	if day := now.In(t.et).Format("2006-01-02"); f.day != day {
		f.day, f.buyVol, f.sellVol = day, 0, 0 // reset each trading day
		f.buckets = map[int64]*bucket{}
	}
	var buy bool
	switch {
	case f.ask > 0 && price >= f.ask:
		buy = true
	case f.bid > 0 && price <= f.bid:
		buy = false
	case f.bid > 0 && f.ask > 0:
		buy = price >= (f.bid+f.ask)/2
	default:
		return // no quote yet — can't classify
	}
	min := now.Unix() / 60
	b := f.buckets[min]
	if b == nil {
		b = &bucket{}
		f.buckets[min] = b
	}
	if buy {
		f.buyVol += size
		b.buy += size
	} else {
		f.sellVol += size
		b.sell += size
	}
	for k := range f.buckets { // drop buckets outside the rolling window
		if k < min-int64(rollWindowMin) {
			delete(f.buckets, k)
		}
	}
}

// Pressure is the public per-symbol order-flow snapshot.
type Pressure struct {
	Symbol      string  `json:"symbol"`
	BuyVol      float64 `json:"buy_vol"`       // day cumulative
	SellVol     float64 `json:"sell_vol"`      // day cumulative
	BuyPct      float64 `json:"buy_pct"`       // day, 0..100
	RollBuyVol  float64 `json:"roll_buy_vol"`  // last N minutes
	RollSellVol float64 `json:"roll_sell_vol"` // last N minutes
	RollBuyPct  float64 `json:"roll_buy_pct"`  // last N minutes, 0..100
	WindowMin   int     `json:"window_min"`    // N
}

// Snapshot returns the current order-flow estimate for a symbol (day + rolling window).
func (t *Tracker) Snapshot(sym string) Pressure {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := Pressure{Symbol: sym, WindowMin: rollWindowMin}
	f := t.m[sym]
	if f == nil {
		return out
	}
	out.BuyVol, out.SellVol = f.buyVol, f.sellVol
	if day := f.buyVol + f.sellVol; day > 0 {
		out.BuyPct = f.buyVol / day * 100
	}
	curMin := time.Now().Unix() / 60
	var rb, rs float64
	for k, b := range f.buckets {
		if k >= curMin-int64(rollWindowMin)+1 {
			rb += b.buy
			rs += b.sell
		}
	}
	out.RollBuyVol, out.RollSellVol = rb, rs
	if roll := rb + rs; roll > 0 {
		out.RollBuyPct = rb / roll * 100
	}
	return out
}
