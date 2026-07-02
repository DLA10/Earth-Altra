// Package scanner maintains the single shared per-ticker state store that every
// DECEPTICON UI panel reads from. It is driven by Alpaca 1-minute bars (light: ~one
// message per symbol per minute) plus a daily backfill for prior-close and average
// volume. No UI panel computes its own numbers — they all read this store.
package scanner

import (
	"math"
	"sort"
	"sync"
	"time"
)

// regularSessionMinutes is the length of a US regular trading session (09:30–16:00).
const regularSessionMinutes = 390.0

// Bar is a single 1-minute OHLCV bar with VWAP.
type Bar struct {
	Time   int64   `json:"time"` // unix seconds (bar open)
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
	VWAP   float64 `json:"vwap"`
}

// State is the public per-ticker scan record.
type State struct {
	Symbol      string  `json:"symbol"`
	Price       float64 `json:"price"`
	PrevClose   float64 `json:"prev_close"`
	Open        float64 `json:"open"`
	ChgClosePct float64 `json:"chg_close_pct"` // % vs prior close
	ChgOpenPct  float64 `json:"chg_open_pct"`  // % vs today's open
	OR5Pct      float64 `json:"or5_pct"`       // opening-range move, first 5 min
	OR15Pct     float64 `json:"or15_pct"`
	OR20Pct     float64 `json:"or20_pct"`
	Volume      float64 `json:"volume"`     // today cumulative
	AvgVolume   float64 `json:"avg_volume"` // avg daily volume baseline
	RVOL        float64 `json:"rvol"`       // relative volume vs typical-at-this-time
	VWAP        float64 `json:"vwap"`       // session VWAP
	DayHigh     float64 `json:"day_high"`
	DayLow      float64 `json:"day_low"`
	Spread      float64 `json:"spread"`
	Catalyst    string  `json:"catalyst"`
	HasBars     bool    `json:"has_bars"`
	Updated     int64   `json:"updated"`
}

type entry struct {
	st       State
	bars     []Bar // today's session bars (1-minute)
	barIndex map[int64]int
}

// Scanner is the shared store.
type Scanner struct {
	mu       sync.RWMutex
	entries  map[string]*entry
	profiles map[string][]float64 // sym -> cumFrac[390]: expected cumulative volume fraction by minute of the regular session
	et       *time.Location
}

// New creates a Scanner for the given symbols, seeding catalyst strings.
func New(catalysts map[string]string) *Scanner {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	s := &Scanner{entries: map[string]*entry{}, profiles: map[string][]float64{}, et: loc}
	for sym, cat := range catalysts {
		s.entries[sym] = &entry{
			st:       State{Symbol: sym, Catalyst: cat},
			barIndex: map[int64]int{},
		}
	}
	return s
}

// sessionBounds returns the 09:30 and 16:00 ET unix bounds for the ET day of unix time t,
// plus whether that day is a weekday.
func (s *Scanner) sessionBounds(t int64) (start, end int64, weekday bool) {
	n := time.Unix(t, 0).In(s.et)
	open := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, s.et)
	cl := time.Date(n.Year(), n.Month(), n.Day(), 16, 0, 0, 0, s.et)
	wd := n.Weekday() >= time.Monday && n.Weekday() <= time.Friday
	return open.Unix(), cl.Unix(), wd
}

// inRegularSession reports whether a bar-open time falls inside 09:30–16:00 ET on a weekday.
// The scanner stores ONLY regular-session bars so every metric (open, VWAP, day high/low,
// opening-range, RVOL) is deterministically anchored to the 09:30 open — never contaminated
// by pre-market or after-hours prints, regardless of when the process started.
func (s *Scanner) inRegularSession(t int64) bool {
	start, end, wd := s.sessionBounds(t)
	return wd && t >= start && t < end
}

// SetVolumeProfile installs a symbol's intraday cumulative-volume curve: cumFrac[m] is the
// average fraction of a full day's volume traded by minute m of the regular session (0..389).
// Used to make RVOL time-of-day-aware instead of assuming volume is spread evenly.
func (s *Scanner) SetVolumeProfile(sym string, cumFrac []float64) {
	if len(cumFrac) != int(regularSessionMinutes) {
		return
	}
	s.mu.Lock()
	s.profiles[sym] = cumFrac
	if e := s.entries[sym]; e != nil {
		s.recompute(e)
	}
	s.mu.Unlock()
}

// Get returns a copy of a symbol's current scan state (for callers like the AI agent).
func (s *Scanner) Get(sym string) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[sym]; ok {
		return e.st, true
	}
	return State{}, false
}

// SeedDaily sets the prior close and average daily volume from historical daily bars.
func (s *Scanner) SeedDaily(sym string, prevClose, avgVolume float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sym]
	if e == nil {
		return
	}
	e.st.PrevClose = prevClose
	e.st.AvgVolume = avgVolume
	s.recompute(e)
}

// SeedIntraday loads today's 1-minute session bars (from REST) in one shot.
func (s *Scanner) SeedIntraday(sym string, bars []Bar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sym]
	if e == nil {
		return
	}
	for _, b := range bars {
		s.mergeBar(e, b)
	}
	s.recompute(e)
}

// OnBar folds a live 1-minute bar into the store.
func (s *Scanner) OnBar(sym string, t time.Time, open, high, low, clse, volume, vwap float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sym]
	if e == nil {
		return
	}
	s.mergeBar(e, Bar{
		Time:   t.Unix(),
		Open:   open,
		High:   high,
		Low:    low,
		Close:  clse,
		Volume: volume,
		VWAP:   vwap,
	})
	s.recompute(e)
}

// OnQuote updates the bid/ask spread (only for symbols we stream quotes for).
func (s *Scanner) OnQuote(sym string, bid, ask float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[sym]
	if e == nil || bid <= 0 || ask <= 0 {
		return
	}
	e.st.Spread = ask - bid
}

// mergeBar inserts/replaces a bar keyed by its open time (caller holds lock).
func (s *Scanner) mergeBar(e *entry, b Bar) {
	if !s.inRegularSession(b.Time) {
		return // drop pre-market / after-hours bars so metrics anchor to the 09:30 open
	}
	if idx, ok := e.barIndex[b.Time]; ok {
		e.bars[idx] = b
		return
	}
	e.barIndex[b.Time] = len(e.bars)
	e.bars = append(e.bars, b)
	// Keep ordered (bars usually arrive in order; guard anyway).
	if len(e.bars) > 1 && e.bars[len(e.bars)-1].Time < e.bars[len(e.bars)-2].Time {
		sort.Slice(e.bars, func(i, j int) bool { return e.bars[i].Time < e.bars[j].Time })
		for i, bb := range e.bars {
			e.barIndex[bb.Time] = i
		}
	}
}

// recompute derives all metrics from the session bars (caller holds lock).
func (s *Scanner) recompute(e *entry) {
	st := &e.st
	if len(e.bars) == 0 {
		st.HasBars = false
		if st.PrevClose > 0 {
			st.Price = st.PrevClose
		}
		return
	}
	st.HasBars = true
	first := e.bars[0]
	last := e.bars[len(e.bars)-1]

	st.Open = first.Open
	st.Price = last.Close

	var vol, pvSum, high, low float64
	high = math.Inf(-1)
	low = math.Inf(1)
	for _, b := range e.bars {
		vol += b.Volume
		pvSum += b.VWAP * b.Volume
		if b.High > high {
			high = b.High
		}
		if b.Low < low {
			low = b.Low
		}
	}
	st.Volume = vol
	st.DayHigh = high
	st.DayLow = low
	if vol > 0 {
		st.VWAP = pvSum / vol
	} else {
		st.VWAP = last.Close
	}

	if st.PrevClose > 0 {
		st.ChgClosePct = (st.Price - st.PrevClose) / st.PrevClose * 100
	}
	if st.Open > 0 {
		st.ChgOpenPct = (st.Price - st.Open) / st.Open * 100
	}

	// Opening-range moves: price at +5/+15/+20 min vs open.
	st.OR5Pct = s.orMove(e, 5)
	st.OR15Pct = s.orMove(e, 15)
	st.OR20Pct = s.orMove(e, 20)

	// Relative volume: today's cumulative volume vs the EXPECTED cumulative volume by this
	// minute of the session. Uses the symbol's learned intraday volume curve (heavy at the
	// open/close, light midday) when loaded; falls back to a flat fraction until then.
	if st.AvgVolume > 0 {
		if frac := s.expectedVolFracLocked(st.Symbol, first.Time, last.Time); frac > 0 {
			st.RVOL = st.Volume / (st.AvgVolume * frac)
		}
	}

	st.Updated = last.Time
}

// expectedVolFracLocked returns the fraction of a normal day's volume that should have traded
// by the latest bar: from the symbol's intraday volume profile if present, else a flat
// (linear) estimate. Caller holds the lock.
func (s *Scanner) expectedVolFracLocked(sym string, firstBar, lastBar int64) float64 {
	if prof := s.profiles[sym]; len(prof) == int(regularSessionMinutes) {
		start, _, _ := s.sessionBounds(lastBar)
		m := int((lastBar - start) / 60)
		if m < 0 {
			m = 0
		}
		if m >= len(prof) {
			m = len(prof) - 1
		}
		if prof[m] > 0 {
			return prof[m]
		}
	}
	// Fallback (no profile yet): the old flat assumption.
	elapsed := float64(lastBar-firstBar)/60.0 + 1.0
	frac := elapsed / regularSessionMinutes
	if frac <= 0 {
		frac = 1.0 / regularSessionMinutes
	}
	if frac > 1 {
		frac = 1
	}
	return frac
}

// BuildVolumeProfile turns N sessions of regular-hours 1-minute bars (any order, any number
// of days mixed together) into a cumulative-volume curve cumFrac[390]. Exposed so the server
// can build profiles from historical bars and install them via SetVolumeProfile.
func (s *Scanner) BuildVolumeProfile(bars []Bar) []float64 {
	const mins = int(regularSessionMinutes)
	// Group regular-session bars by ET calendar day.
	byDay := map[string][]Bar{}
	for _, b := range bars {
		if !s.inRegularSession(b.Time) {
			continue
		}
		day := time.Unix(b.Time, 0).In(s.et).Format("2006-01-02")
		byDay[day] = append(byDay[day], b)
	}
	sum := make([]float64, mins)
	cnt := make([]int, mins)
	for _, dayBars := range byDay {
		perMin := make([]float64, mins)
		var anchor int64 = -1
		for _, b := range dayBars {
			start, _, _ := s.sessionBounds(b.Time)
			anchor = start
			if m := int((b.Time - start) / 60); m >= 0 && m < mins {
				perMin[m] += b.Volume
			}
		}
		if anchor < 0 {
			continue
		}
		cum := make([]float64, mins)
		run := 0.0
		for m := 0; m < mins; m++ {
			run += perMin[m]
			cum[m] = run
		}
		total := cum[mins-1]
		if total <= 0 {
			continue
		}
		for m := 0; m < mins; m++ {
			sum[m] += cum[m] / total
			cnt[m]++
		}
	}
	out := make([]float64, mins)
	var last float64
	for m := 0; m < mins; m++ {
		if cnt[m] > 0 {
			out[m] = sum[m] / float64(cnt[m])
		} else {
			out[m] = last // carry forward across gaps
		}
		if out[m] < last {
			out[m] = last // enforce monotonic non-decreasing
		}
		last = out[m]
	}
	return out
}

// orMove returns the % move from the open to the close of the bar nearest minute n.
func (s *Scanner) orMove(e *entry, minute int) float64 {
	if len(e.bars) == 0 || e.st.Open <= 0 {
		return 0
	}
	target := e.bars[0].Time + int64(minute*60)
	var px float64
	found := false
	for _, b := range e.bars {
		if b.Time <= target {
			px = b.Close
			found = true
		} else {
			break
		}
	}
	if !found {
		return 0
	}
	return (px - e.st.Open) / e.st.Open * 100
}

// Snapshot returns the current state for all symbols.
func (s *Scanner) Snapshot() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// SessionBars returns today's 1-minute bars plus a cumulative-VWAP line for charts.
func (s *Scanner) SessionBars(sym string) ([]Bar, []VWAPPoint) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.entries[sym]
	if e == nil || len(e.bars) == 0 {
		return nil, nil
	}
	bars := make([]Bar, len(e.bars))
	copy(bars, e.bars)
	line := make([]VWAPPoint, 0, len(bars))
	var pv, vv float64
	for _, b := range bars {
		pv += b.VWAP * b.Volume
		vv += b.Volume
		v := b.Close
		if vv > 0 {
			v = pv / vv
		}
		line = append(line, VWAPPoint{Time: b.Time, Value: v})
	}
	return bars, line
}

// VWAPPoint is one point on the cumulative VWAP line.
type VWAPPoint struct {
	Time  int64   `json:"time"`
	Value float64 `json:"value"`
}

// Mover is one symbol's move from the open at a time mark.
type Mover struct {
	Symbol string  `json:"symbol"`
	Open   float64 `json:"open"`
	Price  float64 `json:"price"`
	Pct    float64 `json:"pct"`
}

// IntervalRank ranks the universe by % move from open at a given minute mark.
type IntervalRank struct {
	Minutes int     `json:"minutes"`
	Elapsed bool    `json:"elapsed"`
	Rising  []Mover `json:"rising"`
	Falling []Mover `json:"falling"`
}

// OpeningAnalysis ranks every tracked symbol by its % move from the session open,
// measured at +15/+30/+45/+60 minutes, from the stored 1-minute session bars. Marks
// that haven't elapsed yet come back with Elapsed=false and empty lists.
func (s *Scanner) OpeningAnalysis(topN int) []IntervalRank {
	marks := []int{5, 15, 30, 45, 60}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]IntervalRank, 0, len(marks))
	for _, m := range marks {
		ir := IntervalRank{Minutes: m}
		movers := make([]Mover, 0, len(s.entries))
		for sym, e := range s.entries {
			if len(e.bars) == 0 {
				continue
			}
			open := e.bars[0].Open
			if open <= 0 {
				continue
			}
			target := e.bars[0].Time + int64(m*60)
			// The mark has elapsed for this symbol only if data reaches it.
			if e.bars[len(e.bars)-1].Time < target {
				continue
			}
			var px float64
			for _, b := range e.bars {
				if b.Time <= target {
					px = b.Close
				} else {
					break
				}
			}
			if px <= 0 {
				continue
			}
			movers = append(movers, Mover{Symbol: sym, Open: open, Price: px, Pct: (px - open) / open * 100})
		}
		if len(movers) > 0 {
			ir.Elapsed = true
			rising := append([]Mover{}, movers...)
			falling := append([]Mover{}, movers...)
			sort.Slice(rising, func(i, j int) bool { return rising[i].Pct > rising[j].Pct })
			sort.Slice(falling, func(i, j int) bool { return falling[i].Pct < falling[j].Pct })
			ir.Rising = topMovers(rising, topN, true)
			ir.Falling = topMovers(falling, topN, false)
		}
		out = append(out, ir)
	}
	return out
}

func topMovers(sorted []Mover, n int, rising bool) []Mover {
	out := make([]Mover, 0, n)
	for _, m := range sorted {
		if len(out) >= n {
			break
		}
		// Only include genuinely up/down names in each list.
		if rising && m.Pct <= 0 {
			break
		}
		if !rising && m.Pct >= 0 {
			break
		}
		out = append(out, m)
	}
	return out
}

// Has reports whether the scanner tracks a symbol.
func (s *Scanner) Has(sym string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[sym]
	return ok
}
