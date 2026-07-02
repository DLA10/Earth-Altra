package signals

import (
	"sort"
	"sync"
	"time"
)

// dailyStats is the per-symbol daily context (volatility + liquidity yardsticks).
type dailyStats struct {
	ATR    float64 // daily ATR(14)
	AvgVol float64 // 20-day average daily volume
}

// Store holds each symbol's regular-session 1-minute bars for TODAY plus daily stats,
// and derives the shared market context from the index symbols (SPY/QQQ). It resets
// itself when a bar from a new session day arrives.
type Store struct {
	mu    sync.RWMutex
	et    *time.Location
	day   string // ET date the bars belong to
	open  int64  // 09:30 ET unix for that day
	bars  map[string][]Bar
	daily map[string]dailyStats
}

// NewStore creates a Store.
func NewStore() *Store {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return &Store{et: loc, bars: map[string][]Bar{}, daily: map[string]dailyStats{}}
}

// SetDaily installs a symbol's daily ATR / average-volume context.
func (s *Store) SetDaily(sym string, atr, avgVol float64) {
	s.mu.Lock()
	s.daily[sym] = dailyStats{ATR: atr, AvgVol: avgVol}
	s.mu.Unlock()
}

// Daily returns a symbol's daily stats (zero values if unseeded).
func (s *Store) Daily(sym string) (atr, avgVol float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.daily[sym]
	return d.ATR, d.AvgVol
}

// sessionInfo returns the 09:30 open and ET date for a bar time.
func (s *Store) sessionInfo(t int64) (open int64, day string, regular bool) {
	n := time.Unix(t, 0).In(s.et)
	if n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
		return 0, "", false
	}
	o := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, s.et)
	c := time.Date(n.Year(), n.Month(), n.Day(), 16, 0, 0, 0, s.et)
	if t < o.Unix() || t >= c.Unix() {
		return 0, "", false
	}
	return o.Unix(), n.Format("2006-01-02"), true
}

// OnBar folds one 1-minute bar in. Non-regular-session bars are dropped so every metric
// anchors to the 09:30 open. Returns the symbol's bars snapshot (nil if dropped) — the
// caller runs detection on it without re-locking.
func (s *Store) OnBar(sym string, t time.Time, o, h, l, c, v float64) []Bar {
	open, day, ok := s.sessionInfo(t.Unix())
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if day != s.day { // new session — today's bars only
		s.day = day
		s.open = open
		s.bars = map[string][]Bar{}
	}
	nb := Bar{Time: t.Unix(), Open: o, High: h, Low: l, Close: c, Volume: v}
	arr := s.bars[sym]
	if n := len(arr); n > 0 && arr[n-1].Time == nb.Time {
		arr[n-1] = nb // replace (authoritative correction)
	} else {
		arr = append(arr, nb)
		if n > 0 && arr[len(arr)-2].Time > nb.Time { // out-of-order guard
			sort.Slice(arr, func(i, j int) bool { return arr[i].Time < arr[j].Time })
		}
	}
	s.bars[sym] = arr
	out := make([]Bar, len(arr))
	copy(out, arr)
	return out
}

// SessionOpen returns the current session's 09:30 unix (0 before any bar arrived).
func (s *Store) SessionOpen() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.open
}

// RVOL returns today's cumulative volume for sym relative to the time-adjusted
// expectation from its 20-day average (flat intraday curve — good enough for gating).
func (s *Store) RVOL(sym string, now int64) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.daily[sym]
	if d.AvgVol <= 0 || s.open == 0 {
		return 0
	}
	var vol float64
	for _, b := range s.bars[sym] {
		vol += b.Volume
	}
	frac := clampF((float64(now-s.open)/60+1)/regularSessionMin, 1.0/regularSessionMin, 1)
	return vol / (d.AvgVol * frac)
}

// Market derives the shared backdrop from QQQ's session bars: OK when QQQ is above its
// VWAP or green from the open. Returns (ok, pctFromOpen). No QQQ data → (true, 0) so the
// engine degrades to symbol-only evidence instead of going silent.
func (s *Store) Market() (bool, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bars := s.bars["QQQ"]
	if len(bars) < 5 {
		return true, 0
	}
	vw := vwapSeries(bars)
	last := bars[len(bars)-1]
	pct := 0.0
	if open := bars[0].Open; open > 0 {
		pct = (last.Close - open) / open * 100
	}
	return last.Close >= vw[len(vw)-1] || pct >= 0, pct
}
