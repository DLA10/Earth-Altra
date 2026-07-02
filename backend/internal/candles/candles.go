// Package candles maintains rolling OHLCV candles per symbol and timeframe, built
// from Alpaca's real-time minute bars and trades. It backfills history via REST so
// charts are populated on first load, then updates the forming candle on each tick.
package candles

import (
	"math"
	"sort"
	"sync"
	"time"
)

// TimeframeMinutes lists the timeframes the UI can toggle between.
var TimeframeMinutes = []int{1, 5, 10}

const (
	// maxTickDeviation rejects a single trade print whose price jumps more than this
	// fraction from the last good price — erroneous/odd-lot prints draw fake wicks the
	// market never actually traded. 0.20 = 20%, far beyond any real tick-to-tick move
	// for the liquid US equities this app trades, so it only catches bad data.
	maxTickDeviation = 0.20
	// tickGapResetSec: if it's been longer than this since the last accepted tick, the
	// reference is stale (overnight, halt, session boundary) so we accept the next tick
	// unconditionally instead of mistaking a legitimate gap for a bad print.
	tickGapResetSec = 300
)

// Candle is a single OHLCV bar. Time is the bar's opening time (UTC, unix seconds).
type Candle struct {
	Time   int64   `json:"time"` // unix seconds (bar open)
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

// Update is broadcast to clients whenever a candle changes.
type Update struct {
	Symbol    string `json:"symbol"`
	Timeframe int    `json:"timeframe"` // minutes
	Candle    Candle `json:"candle"`
}

// series holds the candles for one (symbol, timeframe).
type series struct {
	tf      int
	byTime  map[int64]*Candle
	order   []int64 // sorted bar open times
	maxKeep int
	ref     float64 // last accepted price, for the bad-tick guard
	refTime int64   // unix time of the last accepted price
}

func newSeries(tf, maxKeep int) *series {
	return &series{tf: tf, byTime: map[int64]*Candle{}, maxKeep: maxKeep}
}

// bucket returns the bar-open unix-second for time t under this timeframe.
func (s *series) bucket(t time.Time) int64 {
	secs := int64(s.tf) * 60
	return (t.Unix() / secs) * secs
}

// apply folds a live trade tick (price + volume) into the series and returns the
// affected candle plus whether it changed. Bad prints (non-positive, or a wild jump
// from the last good price within a short window) are dropped so they can't draw a
// fake wick.
func (s *series) apply(t time.Time, price, volume float64) (Candle, bool) {
	if price <= 0 {
		return Candle{}, false
	}
	ts := t.Unix()
	if s.ref > 0 && ts-s.refTime <= tickGapResetSec {
		if math.Abs(price-s.ref)/s.ref > maxTickDeviation {
			return Candle{}, false // erroneous print — ignore
		}
	}
	key := s.bucket(t)
	c, ok := s.byTime[key]
	if !ok {
		c = &Candle{Time: key, Open: price, High: price, Low: price, Close: price, Volume: volume}
		s.byTime[key] = c
		s.order = append(s.order, key)
		sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
		s.trim()
		s.ref, s.refTime = price, ts
		return *c, true
	}
	if price > c.High {
		c.High = price
	}
	if price < c.Low {
		c.Low = price
	}
	c.Close = price
	c.Volume += volume
	s.ref, s.refTime = price, ts
	return *c, true
}

// seed inserts a fully-formed historical candle (from backfill) without mutating it
// like a live tick. Used when loading REST history.
func (s *series) seed(c Candle) {
	if _, ok := s.byTime[c.Time]; !ok {
		s.order = append(s.order, c.Time)
	}
	cc := c
	s.byTime[c.Time] = &cc
}

func (s *series) finalizeSeed() {
	sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
	s.trim()
	if n := len(s.order); n > 0 {
		last := s.byTime[s.order[n-1]]
		s.ref, s.refTime = last.Close, last.Time
	}
}

func (s *series) trim() {
	if len(s.order) <= s.maxKeep {
		return
	}
	drop := s.order[:len(s.order)-s.maxKeep]
	s.order = s.order[len(s.order)-s.maxKeep:]
	for _, k := range drop {
		delete(s.byTime, k)
	}
}

func (s *series) snapshot() []Candle {
	out := make([]Candle, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, *s.byTime[k])
	}
	return out
}

// Engine maintains all series and emits updates via the OnUpdate callback.
type Engine struct {
	mu       sync.RWMutex
	maxKeep  int
	series   map[string]map[int]*series // symbol -> tf -> series
	OnUpdate func(Update)
}

// NewEngine builds an engine for the given symbols, keeping maxKeep candles each.
func NewEngine(symbols []string, maxKeep int) *Engine {
	e := &Engine{maxKeep: maxKeep, series: map[string]map[int]*series{}}
	for _, sym := range symbols {
		e.addSymbolLocked(sym)
	}
	return e
}

// AddSymbol registers a new symbol at runtime (idempotent), so subsequent OnBar/
// OnTrade/Seed calls populate its candles. Safe for concurrent use.
func (e *Engine) AddSymbol(sym string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addSymbolLocked(sym)
}

// Tracks reports whether the engine already maintains series for a symbol (so callers
// can skip re-backfilling an already-live name).
func (e *Engine) Tracks(sym string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.series[sym]
	return ok
}

func (e *Engine) addSymbolLocked(sym string) {
	if _, ok := e.series[sym]; ok {
		return
	}
	e.series[sym] = map[int]*series{}
	for _, tf := range TimeframeMinutes {
		e.series[sym][tf] = newSeries(tf, e.maxKeep)
	}
}

func (e *Engine) emit(sym string, c Candle, tf int) {
	if e.OnUpdate != nil {
		e.OnUpdate(Update{Symbol: sym, Timeframe: tf, Candle: c})
	}
}

// OnTrade folds a live trade into every timeframe for the symbol.
func (e *Engine) OnTrade(sym string, t time.Time, price, size float64) {
	e.mu.Lock()
	tfs, ok := e.series[sym]
	if !ok {
		e.mu.Unlock()
		return
	}
	type ev struct {
		c  Candle
		tf int
	}
	var events []ev
	for tf, s := range tfs {
		c, changed := s.apply(t, price, size)
		if changed {
			events = append(events, ev{c, tf})
		}
	}
	e.mu.Unlock()
	for _, e2 := range events {
		e.emit(sym, e2.c, e2.tf)
	}
}

// OnBar folds an Alpaca 1-minute bar in. We use the bar close as the price and add
// its volume; this keeps aggregation correct even when individual trades are sparse.
func (e *Engine) OnBar(sym string, t time.Time, open, high, low, clse, volume float64) {
	e.mu.Lock()
	tfs, ok := e.series[sym]
	if !ok {
		e.mu.Unlock()
		return
	}
	type ev struct {
		c  Candle
		tf int
	}
	var events []ev
	for tf, s := range tfs {
		key := s.bucket(t)
		c, exists := s.byTime[key]
		if !exists {
			c = &Candle{Time: key, Open: open, High: high, Low: low, Close: clse, Volume: volume}
			s.byTime[key] = c
			s.order = append(s.order, key)
			sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
			s.trim()
		} else {
			if high > c.High {
				c.High = high
			}
			if low < c.Low {
				c.Low = low
			}
			c.Close = clse
			c.Volume += volume
		}
		// Bars are aggregated/cleaned by Alpaca, so trust them and refresh the bad-tick
		// reference for subsequent live trades. Use the bar's actual time, not the bucket
		// open (a 10-minute bucket's open can be old enough to look like a stale gap and
		// silently disable the wild-jump guard for the next trade).
		if clse > 0 {
			s.ref, s.refTime = clse, t.Unix()
		}
		events = append(events, ev{*c, tf})
	}
	e.mu.Unlock()
	for _, e2 := range events {
		e.emit(sym, e2.c, e2.tf)
	}
}

// Seed loads backfilled 1-minute candles and rolls them up into all timeframes.
func (e *Engine) Seed(sym string, minuteBars []Candle) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tfs, ok := e.series[sym]
	if !ok {
		return
	}
	for tf, s := range tfs {
		if tf == 1 {
			for _, b := range minuteBars {
				s.seed(b)
			}
			s.finalizeSeed()
			continue
		}
		// Roll 1-minute bars up into this timeframe.
		rolled := map[int64]*Candle{}
		var keys []int64
		for _, b := range minuteBars {
			key := s.bucket(time.Unix(b.Time, 0).UTC())
			c, exists := rolled[key]
			if !exists {
				cc := Candle{Time: key, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
				rolled[key] = &cc
				keys = append(keys, key)
				continue
			}
			if b.High > c.High {
				c.High = b.High
			}
			if b.Low < c.Low {
				c.Low = b.Low
			}
			c.Close = b.Close
			c.Volume += b.Volume
		}
		for _, k := range keys {
			s.seed(*rolled[k])
		}
		s.finalizeSeed()
	}
}

// Snapshot returns the current candles for a symbol+timeframe.
func (e *Engine) Snapshot(sym string, tf int) []Candle {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if tfs, ok := e.series[sym]; ok {
		if s, ok := tfs[tf]; ok {
			return s.snapshot()
		}
	}
	return nil
}
