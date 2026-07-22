// Package regime is the SHADOW regime detector (operator-approved 2026-07-22,
// REGIME_DETECTOR_STUDY.md). It publishes a daily prediction — does this AFTERNOON
// favor momentum (TREND) or dip-reversion (CHOP)? — using detector D3 "morning probe":
// replay two fixed micro-strategies on the morning (10:05–11:25) and bet the winner
// repeats in the afternoon. PURE OBSERVER: reads candle snapshots, writes a journal,
// never places an order and never gates a desk. Its live hit rate is reviewed weekly;
// only a sustained ≥60% earns it any authority (a separate, explicit change).
//
// Probe definitions are the leak-free v2 semantics (entries in [t0,t1), positions
// managed through t1+maxhold) — exact port of the repaired regime_panel.py.
package regime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"live-optimus/backend/internal/candles"
)

const slice = 1500.0

type grid struct{ c, o, h, l [390]float64 }

type Detector struct {
	engine *candles.Engine
	etz    *time.Location
	dir    string
	syms   []string
	barsFn BarsFn

	day      string
	predDone bool
	outDone  bool
	pred     int // -1 unknown, 0 chop, 1 trend
}

// BarsFn fetches official 1-min bars for many symbols in [start, end). The live candle
// engine only carries the ~40 trade-subscribed execution/watchlist names — nowhere near
// the 534-name universe the probes were validated on (2026-07-22: the first live
// prediction died with "only 44 symbols with morning bars") — so the REST multi-bar
// endpoint is the primary source; the engine path below remains as a fallback.
type BarsFn func(symbols []string, start, end time.Time) map[string][]candles.Candle

// SetBarsFn wires the REST bar source; call before Start.
func (d *Detector) SetBarsFn(fn BarsFn) { d.barsFn = fn }

func New(engine *candles.Engine, etz *time.Location, dataDir string, syms []string) *Detector {
	return &Detector{engine: engine, etz: etz, dir: filepath.Join(dataDir, "regime"),
		syms: syms, pred: -1}
}

func (d *Detector) Start(ctx context.Context) {
	if d == nil || d.engine == nil || len(d.syms) == 0 {
		return
	}
	_ = os.MkdirAll(d.dir, 0755)
	log.Printf("regime: shadow D3 morning-probe ON (log-only) — predict ~11:31 ET, outcome ~16:05 ET → data/regime/<day>.jsonl")
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.cycle()
			}
		}
	}()
}

func (d *Detector) cycle() {
	now := time.Now().In(d.etz)
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return
	}
	day := now.Format("2006-01-02")
	if day != d.day {
		d.day, d.predDone, d.outDone, d.pred = day, false, false, -1
	}
	hm := now.Hour()*60 + now.Minute()
	if !d.predDone && hm >= 11*60+31 && hm < 16*60 {
		d.predDone = true
		d.runPrediction(day, hm <= 11*60+45)
	}
	if !d.outDone && hm >= 16*60+5 {
		d.outDone = true
		d.runOutcome(day)
	}
}

// grids builds RTH minute grids (close ffilled; o/h/l raw) using bars strictly BEFORE
// minute uptoMin of the given ET day. Source: one batched REST fetch when barsFn is
// wired (the validated path), else per-symbol engine snapshots.
func (d *Detector) grids(day string, uptoMin int) map[string]*grid {
	src := make(map[string][]candles.Candle, len(d.syms))
	if dayT, err := time.ParseInLocation("2006-01-02", day, d.etz); d.barsFn != nil && err == nil {
		endMin := uptoMin
		if endMin > 960 {
			endMin = 960
		}
		src = d.barsFn(d.syms, dayT.Add(570*time.Minute), dayT.Add(time.Duration(endMin)*time.Minute))
	} else {
		for _, sym := range d.syms {
			src[sym] = d.engine.Snapshot(sym, 1)
		}
	}
	out := make(map[string]*grid, len(src))
	for sym, bars := range src {
		if len(bars) == 0 {
			continue
		}
		g := &grid{}
		for i := range g.c {
			g.c[i], g.o[i], g.h[i], g.l[i] = math.NaN(), math.NaN(), math.NaN(), math.NaN()
		}
		n := 0
		for _, b := range bars {
			bt := time.Unix(b.Time, 0).In(d.etz)
			if bt.Format("2006-01-02") != day {
				continue
			}
			m := bt.Hour()*60 + bt.Minute()
			if m < 570 || m >= uptoMin || m >= 960 {
				continue
			}
			i := m - 570
			g.c[i], g.o[i], g.h[i], g.l[i] = b.Close, b.Open, b.High, b.Low
			n++
		}
		if n < 45 {
			continue
		}
		last := math.NaN()
		for i := range g.c {
			if math.IsNaN(g.c[i]) {
				g.c[i] = last
			} else {
				last = g.c[i]
			}
		}
		out[sym] = g
	}
	return out
}

// medR30 = per-minute cross-sectional median of the 30-minute log return.
func medR30(gs map[string]*grid, upto int) []float64 {
	med := make([]float64, upto)
	buf := make([]float64, 0, len(gs))
	for m := 0; m < upto; m++ {
		med[m] = math.NaN()
		if m < 30 {
			continue
		}
		buf = buf[:0]
		for _, g := range gs {
			a, b := g.c[m], g.c[m-30]
			if !math.IsNaN(a) && !math.IsNaN(b) && a > 0 && b > 0 {
				buf = append(buf, math.Log(a)-math.Log(b))
			}
		}
		if len(buf) < 50 {
			continue
		}
		sort.Float64s(buf)
		med[m] = buf[len(buf)/2]
	}
	return med
}

// probeR — leak-free v2: entries in [t0,t1), managed through min(t1+maxhold, 385).
func probeR(gs map[string]*grid, med []float64, t0, t1, maxhold int) (float64, int) {
	manageTo := t1 + maxhold
	if manageTo > 385 {
		manageTo = 385
	}
	pnl, n := 0.0, 0
	for _, g := range gs {
		cool := -1
		ei := -1
		e := 0.0
		for t := t0; t <= manageTo && t < 390; t++ {
			c := g.c[t]
			if math.IsNaN(c) || t < 33 {
				continue
			}
			if ei < 0 {
				if t >= t1 || t < cool || t+1 >= len(med) {
					continue
				}
				if math.IsNaN(med[t]) || math.Abs(med[t]) > 0.0015 {
					continue
				}
				c30 := g.c[t-30]
				if math.IsNaN(c30) || c30 <= 0 || c <= 0 {
					continue
				}
				idio := (math.Log(c) - math.Log(c30)) - med[t]
				if idio > -0.010 {
					continue
				}
				// stabilization: current low not below the prior 3 lows
				lows := [4]float64{g.l[t-3], g.l[t-2], g.l[t-1], g.l[t]}
				bad := false
				minPrior := math.Inf(1)
				for k := 0; k < 3; k++ {
					if math.IsNaN(lows[k]) {
						bad = true
						break
					}
					if lows[k] < minPrior {
						minPrior = lows[k]
					}
				}
				if bad || math.IsNaN(lows[3]) || minPrior > lows[3] {
					continue
				}
				ent := c
				if t+1 < 390 && !math.IsNaN(g.o[t+1]) {
					ent = g.o[t+1]
				}
				if ent <= 0 || int(slice/ent) < 1 {
					continue
				}
				e, ei = ent, t+1
			} else {
				xp := math.NaN()
				if !math.IsNaN(g.l[t]) && g.l[t] <= e*0.996 {
					xp = e * 0.996
				} else if !math.IsNaN(g.h[t]) && g.h[t] >= e*1.004 {
					xp = e * 1.004
				} else if t-ei >= maxhold || t >= manageTo {
					xp = c
				}
				if !math.IsNaN(xp) {
					pnl += (xp - e) * float64(int(slice/e))
					n++
					cool = t + 30
					ei = -1
				}
			}
		}
		if ei >= 0 { // still open at the boundary
			xp := g.c[manageTo]
			if !math.IsNaN(xp) {
				pnl += (xp - e) * float64(int(slice/e))
				n++
			}
		}
	}
	return pnl, n
}

// probeM — top-5 movers rankA→rankB, entered at entryI, 1.5%→0.5% trail, cut at cutI.
func probeM(gs map[string]*grid, rankA, rankB, entryI, cutI int) (float64, int) {
	type cand struct {
		pct float64
		g   *grid
	}
	var cands []cand
	for _, g := range gs {
		a, b := g.c[rankA], g.c[rankB]
		if math.IsNaN(a) || math.IsNaN(b) || a <= 0 || b < 5 {
			continue
		}
		cands = append(cands, cand{b/a - 1, g})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].pct > cands[j].pct })
	if len(cands) > 5 {
		cands = cands[:5]
	}
	pnl, n := 0.0, 0
	for _, cd := range cands {
		g := cd.g
		e := g.c[entryI]
		if !math.IsNaN(g.o[entryI]) {
			e = g.o[entryI]
		}
		if math.IsNaN(e) || e <= 0 || int(slice/e) < 1 {
			continue
		}
		peak, xp := e, math.NaN()
		for t := entryI; t <= cutI && t < 390; t++ {
			trail := 0.015
			if peak >= e*1.015 {
				trail = 0.005
			}
			if !math.IsNaN(g.l[t]) && g.l[t] <= peak*(1-trail) {
				xp = peak * (1 - trail)
				break
			}
			if !math.IsNaN(g.h[t]) && g.h[t] > peak {
				peak = g.h[t]
			}
		}
		if math.IsNaN(xp) {
			xp = g.c[cutI]
			if math.IsNaN(xp) {
				xp = e
			}
		}
		pnl += (xp - e) * float64(int(slice/e))
		n++
	}
	return pnl, n
}

func (d *Detector) journal(day string, rec map[string]interface{}) {
	rec["t"] = time.Now().In(d.etz).Format("15:04:05")
	f, err := os.OpenFile(filepath.Join(d.dir, day+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(rec); err == nil {
		f.Write(append(b, '\n'))
	}
}

func (d *Detector) runPrediction(day string, live bool) {
	gs := d.grids(day, 690) // strictly before 11:30
	if len(gs) < 100 {
		d.journal(day, map[string]interface{}{"type": "error", "note": fmt.Sprintf("only %d symbols with morning bars", len(gs))})
		return
	}
	med := medR30(gs, 120)
	rAM, rn := probeR(gs, med, 35, 90, 25)
	mAM, mn := probeM(gs, 0, 35, 36, 115)
	d.pred = 0
	if mAM > rAM {
		d.pred = 1
	}
	d.journal(day, map[string]interface{}{"type": "prediction",
		"pred": map[int]string{0: "CHOP", 1: "TREND"}[d.pred], "live": live,
		"pnl_M_am": round2(mAM), "pnl_R_am": round2(rAM), "n_M": mn, "n_R": rn})
	log.Printf("regime: %s prediction %s (M_am %+.0f vs R_am %+.0f) [shadow]",
		day, map[int]string{0: "CHOP", 1: "TREND"}[d.pred], mAM, rAM)
}

func (d *Detector) runOutcome(day string) {
	gs := d.grids(day, 960)
	if len(gs) < 100 {
		return
	}
	med := medR30(gs, 390)
	rPM, _ := probeR(gs, med, 150, 330, 30)
	mPM, _ := probeM(gs, 60, 150, 151, 385)
	label := 0
	if mPM > rPM {
		label = 1
	}
	rec := map[string]interface{}{"type": "outcome",
		"label": map[int]string{0: "CHOP", 1: "TREND"}[label],
		"pnl_M_pm": round2(mPM), "pnl_R_pm": round2(rPM),
		"decisive": math.Abs(mPM-rPM) >= 20}
	if d.pred >= 0 {
		rec["hit"] = d.pred == label
	}
	d.journal(day, rec)
	log.Printf("regime: %s outcome %s (M_pm %+.0f vs R_pm %+.0f) hit=%v [shadow]",
		day, rec["label"], mPM, rPM, rec["hit"])
}

// Report powers /api/regime: today's records + trailing hit rate over prior days.
func (d *Detector) Report() interface{} {
	files, _ := filepath.Glob(filepath.Join(d.dir, "*.jsonl"))
	sort.Strings(files)
	if len(files) > 30 {
		files = files[len(files)-30:]
	}
	hits, tot := 0, 0
	var today []map[string]interface{}
	for _, fp := range files {
		b, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		isToday := d.day != "" && filepath.Base(fp) == d.day+".jsonl"
		for _, ln := range splitLines(b) {
			var rec map[string]interface{}
			if json.Unmarshal(ln, &rec) != nil {
				continue
			}
			if rec["type"] == "outcome" {
				if h, ok := rec["hit"].(bool); ok {
					tot++
					if h {
						hits++
					}
				}
			}
			if isToday {
				today = append(today, rec)
			}
		}
	}
	hr := 0.0
	if tot > 0 {
		hr = float64(hits) / float64(tot) * 100
	}
	return map[string]interface{}{"shadow": true, "detector": "D3 morning-probe",
		"hit_rate_30d": round2(hr), "scored_days": tot, "today": today}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
