// Package surger runs the SURGER v2 experiment: three continuation detectors validated
// across four backtest windows (2026-03..07, 97 sessions — see SURGER_V2.md), deployed
// LIVE on the dip+rise paper account with strict order attribution (srg1_/srg2_/srg3_
// client-order-id prefixes) so the three variants and the dip+rise desk can never be
// confused with each other.
//
// This file is the detector math: an exact Go port of the validated Python harness
// (scratchpad surger_lab.py feats_basic + the C2 / C1 / SPECTRAL definitions), including
// its warm-up semantics: a gate whose window isn't full yet simply cannot fire, exactly
// like NaN comparisons in numpy.
package surger

import (
	"fmt"
	"math"
	"time"
)

// Variant ids (array indices) — order is also entry priority when several fire at once.
const (
	VarC2       = 0 // composite AND fresh CUSUM break (the primary/winner spec)
	VarC1       = 1 // composite AND Fourier trend-purity >= 2
	VarSpectral = 2 // strict purity >= 3 + flow + size (standalone corroborator)
	NumVariants = 3
)

var VariantNames = [NumVariants]string{"C2 cusum", "C1 purity", "SPECTRAL"}
var VariantCoid = [NumVariants]string{"srg1", "srg2", "srg3"}

// series is one symbol's completed 1-min bars for TODAY (bars arrive from the Alpaca
// stream after the minute closes, so there is no forming-bar skew by construction).
type series struct {
	day    string
	minute []int   // ET minutes-of-day, one per bar
	o, h   []float64
	l, c   []float64
	v      []float64
	r1     []float64 // 1-min log returns (r1[0] = 0)
	lc     []float64 // log closes
	cumPV  []float64
	cumV   []float64
	upV    []float64 // cumulative up-volume (volume on bars with r1 > 0)
	absR   []float64 // cumulative |r1|

	cusumS    float64
	cusumFire int // index of the most recent CUSUM fire (-1 = never today)

	// 30-bar-normalized twin used ONLY by the early-mode detector (bars 35..119)
	cusumS30    float64
	cusumFire30 int
}

func newSeries(day string) *series {
	return &series{day: day, cusumFire: -1, cusumFire30: -1}
}

// append adds one completed bar and updates the CUSUM state.
func (s *series) append(minute int, o, h, l, c, v float64) {
	if c <= 0 {
		return
	}
	n := len(s.c)
	s.minute = append(s.minute, minute)
	s.o = append(s.o, o)
	s.h = append(s.h, h)
	s.l = append(s.l, l)
	s.c = append(s.c, c)
	s.v = append(s.v, v)
	lc := math.Log(c)
	s.lc = append(s.lc, lc)
	r := 0.0
	if n > 0 {
		r = lc - s.lc[n-1]
	}
	s.r1 = append(s.r1, r)
	pv := c * v
	up := 0.0
	if r > 0 {
		up = v
	}
	if n == 0 {
		s.cumPV = append(s.cumPV, pv)
		s.cumV = append(s.cumV, v)
		s.upV = append(s.upV, up)
		s.absR = append(s.absR, math.Abs(r))
	} else {
		s.cumPV = append(s.cumPV, s.cumPV[n-1]+pv)
		s.cumV = append(s.cumV, s.cumV[n-1]+v)
		s.upV = append(s.upV, s.upV[n-1]+up)
		s.absR = append(s.absR, s.absR[n-1]+math.Abs(r))
	}
	// CUSUM drift-break (k=0.75, h=8), z-scored by the trailing 60-bar 1-min stdev.
	t := len(s.c) - 1
	if t >= 60 {
		sd := s.stdR1(t-1, 60)
		if sd < 1e-8 {
			sd = 1e-8
		}
		s.cusumS = math.Max(0, s.cusumS+r/sd-0.75)
		if s.cusumS > 8.0 {
			s.cusumFire = t
			s.cusumS = 0
		}
	}
	// early-mode twin: same break test normalized by the 30-bar stdev, live from bar 30
	if t >= 30 {
		sd := s.stdR1(t-1, 30)
		if math.IsNaN(sd) || sd < 1e-8 {
			sd = 1e-8
		}
		s.cusumS30 = math.Max(0, s.cusumS30+r/sd-0.75)
		if s.cusumS30 > 8.0 {
			s.cusumFire30 = t
			s.cusumS30 = 0
		}
	}
}

func (s *series) sum(cum []float64, t, w int) float64 {
	if t-w < 0 {
		return cum[t]
	}
	return cum[t] - cum[t-w]
}

// stdR1 = population stdev of r1 over the w bars ending at t (inclusive).
func (s *series) stdR1(t, w int) float64 {
	if t+1 < w {
		return math.NaN()
	}
	var m float64
	for i := t - w + 1; i <= t; i++ {
		m += s.r1[i]
	}
	m /= float64(w)
	var v float64
	for i := t - w + 1; i <= t; i++ {
		d := s.r1[i] - m
		v += d * d
	}
	return math.Sqrt(v / float64(w))
}

// stdR5 = population stdev over the last w values of the 5-bar-sum return series,
// matching the harness (which zero-filled the first 4 undefined entries).
func (s *series) stdR5(t, w int) float64 {
	if t+1 < w {
		return math.NaN()
	}
	r5 := func(i int) float64 {
		if i < 5 {
			return 0 // harness parity: np.nan_to_num on the warmup rows
		}
		return s.lc[i] - s.lc[i-5]
	}
	var m float64
	for i := t - w + 1; i <= t; i++ {
		m += r5(i)
	}
	m /= float64(w)
	var v float64
	for i := t - w + 1; i <= t; i++ {
		d := r5(i) - m
		v += d * d
	}
	return math.Sqrt(v / float64(w))
}

// feats are the composite's inputs at bar t. ok=false while any window is warming up.
type feats struct {
	r30, eff, upshare, tstat, vr, vsurge, vwap, purity float64
	purityOK                                           bool
}

func (s *series) features(t int) (feats, bool) {
	var f feats
	if t < 34 || t >= len(s.c) { // vsurge is the earliest-binding long window bar 34
		return f, false
	}
	f.r30 = math.NaN()
	if t >= 30 {
		f.r30 = s.lc[t] - s.lc[t-30]
		f.eff = math.Abs(f.r30) / (s.sum(s.absR, t, 30) + 1e-12)
		vol30 := s.sum(s.cumV, t, 30)
		f.upshare = s.sum(s.upV, t, 30) / (vol30 + 1e-9)
	}
	sd60 := s.stdR1(t, 60)
	f.tstat = math.NaN()
	if !math.IsNaN(sd60) && !math.IsNaN(f.r30) {
		f.tstat = f.r30 / (sd60 + 1e-12)
	}
	sd1 := s.stdR1(t, 120)
	sd5 := s.stdR5(t, 120)
	f.vr = math.NaN()
	if !math.IsNaN(sd1) && !math.IsNaN(sd5) {
		f.vr = (sd5 * sd5) / (5*sd1*sd1 + 1e-15)
	}
	v5 := s.sum(s.cumV, t, 5) / 5
	f.vsurge = math.NaN()
	if t-5 >= 29 {
		v30 := s.sum(s.cumV, t-5, 30) / 30 // harness parity: v30 shifted by 5 bars
		f.vsurge = v5 / (v30 + 1e-9)
	}
	f.vwap = s.cumPV[t] / (s.cumV[t] + 1e-9)
	return f, true
}

// purityAt computes the Fourier trend-purity (low-band vs high-band mean power of the
// last 128 one-minute returns). Lazy: only called once the cheap gates pass.
func (s *series) purityAt(t int) (float64, bool) {
	const W = 128
	if t+1 < W {
		return 0, false
	}
	win := s.r1[t+1-W : t+1]
	var mean float64
	for _, x := range win {
		mean += x
	}
	mean /= W
	// rfft power for bins 1..64 (O(W·W/2) — a few thousand mults, negligible per minute)
	var low, high float64
	var lowN, highN int
	for k := 1; k <= W/2; k++ {
		var re, im float64
		for n := 0; n < W; n++ {
			ang := -2 * math.Pi * float64(k) * float64(n) / W
			x := win[n] - mean
			re += x * math.Cos(ang)
			im += x * math.Sin(ang)
		}
		p := re*re + im*im
		if k >= 1 && k <= 8 {
			low += p
			lowN++
		}
		if k >= 32 {
			high += p
			highN++
		}
	}
	if highN == 0 {
		return 0, false
	}
	return (low / float64(lowN)) / (high/float64(highN) + 1e-15), true
}

// gte is a NaN-safe >= (NaN never passes — numpy comparison parity).
func gte(x, thr float64) bool { return !math.IsNaN(x) && x >= thr }

// composite is the shared quality core (harness comp_v2).
func composite(f feats, close float64) bool {
	return gte(f.eff, 0.55) && gte(f.upshare, 0.60) && gte(f.tstat, 2.0) &&
		gte(f.vr, 1.1) && gte(f.vsurge, 1.5) && !math.IsNaN(f.r30) && f.r30 > 0 &&
		close > f.vwap
}

// earlySignal is the validated 30-min-warmup variant (E2 in the 2026-07-21 early-mode
// study, surger_early_results.json): short-window composite (30-bar eff/upshare/tstat +
// vsurge) ∧ fresh sd30-CUSUM break ∧ VR(5,1|30) ≥ 1.1 ∧ close > VWAP. Active bars
// 35..119 and 10:00-11:29 ET only — the main detectors take over at bar 120. Study:
// 38 trades / 97 sessions, 63.2% WR, green @2bp in 3/4 windows incl. both true OOS
// months; expect ~0.4 fires/day across the universe. Books under C2 (same exit).
func (s *series) earlySignal(t int) (bool, string) {
	if t < 35 || t > 119 {
		return false, ""
	}
	if m := s.minute[t]; m < 10*60 || m > 11*60+29 {
		return false, ""
	}
	if s.cusumFire30 < 0 || t-s.cusumFire30 >= 15 {
		return false, ""
	}
	r30 := s.lc[t] - s.lc[t-30]
	if !(r30 > 0) {
		return false, ""
	}
	eff := math.Abs(r30) / (s.sum(s.absR, t, 30) + 1e-12)
	ups := s.sum(s.upV, t, 30) / (s.sum(s.cumV, t, 30) + 1e-9)
	sd30 := s.stdR1(t, 30)
	tstat := math.NaN()
	if !math.IsNaN(sd30) {
		tstat = r30 / (sd30 + 1e-12)
	}
	vsurge := math.NaN()
	if t-5 >= 29 {
		vsurge = (s.sum(s.cumV, t, 5) / 5) / (s.sum(s.cumV, t-5, 30)/30 + 1e-9)
	}
	sd5 := s.stdR5(t, 30)
	vr := math.NaN()
	if !math.IsNaN(sd30) && !math.IsNaN(sd5) {
		vr = (sd5 * sd5) / (5*sd30*sd30 + 1e-15)
	}
	vwap := s.cumPV[t] / (s.cumV[t] + 1e-9)
	if gte(eff, 0.55) && gte(ups, 0.60) && gte(tstat, 2.0) && gte(vsurge, 1.5) &&
		gte(vr, 1.1) && s.c[t] > vwap {
		why := fmt.Sprintf("early: close=%.2f r30=%+.2f%% eff=%.2f ups=%.2f tstat30=%.2f vr30=%.2f vsurge=%.2f cusum30_age=%d vwap=%.2f",
			s.c[t], r30*100, eff, ups, tstat, vr, vsurge, t-s.cusumFire30, vwap)
		return true, why
	}
	return false, ""
}

// signals evaluates all three variants at the LAST completed bar. Returned array is
// indexed by variant id; the string is a feature snapshot for the journal (only built
// when something fired — every logged signal carries the numbers that produced it, so
// the live journal doubles as the learning dataset). Purity is computed lazily.
func (s *series) signals() ([NumVariants]bool, string) {
	var out [NumVariants]bool
	t := len(s.c) - 1
	// early mode: before the main windows are full (bar 120), only the validated
	// short-window variant may fire; it enters C2's book with an "early:" journal tag.
	if t <= 119 {
		if fire, why := s.earlySignal(t); fire {
			out[VarC2] = true
			return out, why
		}
		return out, ""
	}
	f, ok := s.features(t)
	if !ok {
		return out, ""
	}
	close := s.c[t]
	comp := composite(f, close)

	needPurity := comp || (gte(f.r30, 0.004) && gte(f.upshare, 0.55) && close > f.vwap)
	purity, purityOK := 0.0, false
	if needPurity {
		purity, purityOK = s.purityAt(t)
	}

	if comp && s.cusumFire >= 0 && t-s.cusumFire < 15 {
		out[VarC2] = true
	}
	if comp && purityOK && purity >= 2.0 {
		out[VarC1] = true
	}
	if purityOK && purity >= 3.0 && gte(f.r30, 0.004) && gte(f.upshare, 0.55) && close > f.vwap {
		out[VarSpectral] = true
	}
	why := ""
	if out[VarC2] || out[VarC1] || out[VarSpectral] {
		cus := -1
		if s.cusumFire >= 0 {
			cus = t - s.cusumFire
		}
		why = fmt.Sprintf("close=%.2f r30=%+.2f%% eff=%.2f ups=%.2f tstat=%.2f vr=%.2f vsurge=%.2f purity=%.2f cusum_age=%d vwap=%.2f",
			close, f.r30*100, f.eff, f.upshare, f.tstat, f.vr, f.vsurge, purity, cus, f.vwap)
	}
	return out, why
}

// etMinute converts a bar timestamp to ET minutes-of-day.
func etMinute(t time.Time, etz *time.Location) int {
	e := t.In(etz)
	return e.Hour()*60 + e.Minute()
}
