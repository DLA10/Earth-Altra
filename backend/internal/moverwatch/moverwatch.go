// Package moverwatch is a SHADOW recorder for the Movers page's "📈 Risers" table
// (operator request 2026-07-22). It re-derives the exact table the frontend renders —
// same membership filter, same transparent 0–100 riser signal, same pinned-first
// top-12 — and journals it on a 15-minute clock: 09:45 ET (the operator's 2:45 PM,
// 15 minutes after the open), then every quarter-hour through 16:00.
//
// Any symbol whose Signal column shows GREEN (score ≥ 70) enters the day's tracked
// set: its price is recorded at every mark from then on (even after it leaves the
// table), and the marks BEFORE it went green are backfilled from the scanner's
// session bars — so a name that only turns green at 11:15 still shows what it cost
// at 09:45. The formula constants are copied from frontend/src/Movers.tsx and must
// not drift from it; the page itself is untouched.
//
// PURE OBSERVER: writes data/moverwatch/<day>.jsonl, places no orders, subscribes to
// nothing new (reads the scanner's existing store), changes no UI.
package moverwatch

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

	"live-optimus/backend/internal/scanner"
)

const (
	moveMin    = 1.5 // % from open to count as a mover (Movers.tsx MOVE_MIN)
	greenScore = 70  // sigColor threshold for "pos" (green) in Movers.tsx
	tableSize  = 12
	firstMark  = 9*60 + 45 // 09:45 ET
	lastMark   = 16 * 60   // 16:00 ET
)

type track struct {
	FirstMark  string  `json:"first_mark"`
	FirstScore int     `json:"first_score"`
	FirstPx    float64 `json:"first_px"`
	LastPx     float64 `json:"last_px"`
}

type Recorder struct {
	scn   *scanner.Scanner
	etz   *time.Location
	dir   string
	ownFn func() map[string]bool

	day       string
	marksDone map[int]bool
	tracked   map[string]*track
}

func New(scn *scanner.Scanner, etz *time.Location, dataDir string, ownFn func() map[string]bool) *Recorder {
	return &Recorder{scn: scn, etz: etz, dir: filepath.Join(dataDir, "moverwatch"),
		ownFn: ownFn, marksDone: map[int]bool{}, tracked: map[string]*track{}}
}

func (r *Recorder) Start(ctx context.Context) {
	if r == nil || r.scn == nil {
		return
	}
	_ = os.MkdirAll(r.dir, 0755)
	log.Printf("moverwatch: shadow Risers recorder ON (log-only) — table + green-signal prices every 15 min 09:45–16:00 ET → data/moverwatch/<day>.jsonl")
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.cycle()
			}
		}
	}()
}

func (r *Recorder) cycle() {
	now := time.Now().In(r.etz)
	if wd := now.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return
	}
	day := now.Format("2006-01-02")
	if day != r.day {
		r.day = day
		r.marksDone = map[int]bool{}
		r.tracked = map[string]*track{}
	}
	hm := now.Hour()*60 + now.Minute()
	if hm < firstMark || hm > lastMark || hm%15 != 0 || r.marksDone[hm] {
		return
	}
	r.marksDone[hm] = true
	r.record(day, hm)
}

// vwapGap and rangePos mirror Movers.tsx exactly.
func vwapGap(s scanner.State) float64 {
	if s.VWAP > 0 {
		return (s.Price - s.VWAP) / s.VWAP * 100
	}
	return 0
}

func rangePos(s scanner.State) float64 {
	if s.DayHigh > s.DayLow {
		return (s.Price - s.DayLow) / (s.DayHigh - s.DayLow)
	}
	return 0.5
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func riserSignal(s scanner.State) int {
	v := 0.35*clamp01(s.RVOL/4) + 0.25*clamp01(vwapGap(s)/2) +
		0.20*clamp01(rangePos(s)) + 0.20*clamp01(abs(s.ChgOpenPct)/4)
	return int(v*100 + 0.5)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func markLabel(hm int) string { return fmt.Sprintf("%02d:%02d", hm/60, hm%60) }

func (r *Recorder) record(day string, hm int) {
	label := markLabel(hm)
	states := r.scn.Snapshot()
	own := map[string]bool{}
	if r.ownFn != nil {
		own = r.ownFn()
	}

	type row struct {
		st    scanner.State
		score int
		pin   bool
	}
	var rows []row
	for _, s := range states {
		if !s.HasBars || s.Price <= 0 || s.ChgOpenPct < moveMin || vwapGap(s) <= -0.1 {
			continue
		}
		rows = append(rows, row{s, riserSignal(s), own[s.Symbol]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].pin != rows[j].pin {
			return rows[i].pin
		}
		return rows[i].score > rows[j].score
	})
	if len(rows) > tableSize {
		rows = rows[:tableSize]
	}

	tbl := make([]map[string]interface{}, 0, len(rows))
	for _, rw := range rows {
		tbl = append(tbl, map[string]interface{}{
			"sym": rw.st.Symbol, "score": rw.score, "green": rw.score >= greenScore,
			"px": round2(rw.st.Price), "chg_open": round2(rw.st.ChgOpenPct),
			"rvol": round2(rw.st.RVOL), "vwap_gap": round2(vwapGap(rw.st)), "pin": rw.pin})
	}
	r.journal(day, map[string]interface{}{"type": "table", "mark": label, "rows": tbl})

	// New green names: start tracking + backfill the marks they missed.
	for _, rw := range rows {
		if rw.score < greenScore {
			continue
		}
		sym := rw.st.Symbol
		if _, ok := r.tracked[sym]; ok {
			continue
		}
		r.tracked[sym] = &track{FirstMark: label, FirstScore: rw.score, FirstPx: rw.st.Price}
		r.journal(day, map[string]interface{}{"type": "green_new", "mark": label,
			"sym": sym, "score": rw.score, "px": round2(rw.st.Price)})
		for m := firstMark; m < hm; m += 15 {
			if px := r.priceAt(sym, m); px > 0 {
				r.journal(day, map[string]interface{}{"type": "px", "mark": markLabel(m),
					"sym": sym, "px": round2(px), "backfill": true})
			}
		}
	}

	// Price series for every tracked name (on or off the table).
	live := map[string]float64{}
	for _, s := range states {
		live[s.Symbol] = s.Price
	}
	for sym, tr := range r.tracked {
		px := live[sym]
		if px <= 0 {
			px = r.priceAt(sym, hm)
		}
		if px <= 0 {
			continue
		}
		tr.LastPx = px
		r.journal(day, map[string]interface{}{"type": "px", "mark": label, "sym": sym,
			"px": round2(px), "chg_from_green": round2((px/tr.FirstPx - 1) * 100)})
	}

	if hm == lastMark {
		sum := map[string]interface{}{}
		for sym, tr := range r.tracked {
			sum[sym] = map[string]interface{}{"first_mark": tr.FirstMark,
				"first_px": round2(tr.FirstPx), "close_px": round2(tr.LastPx),
				"chg_pct": round2((tr.LastPx/tr.FirstPx - 1) * 100)}
		}
		r.journal(day, map[string]interface{}{"type": "eod", "tracked": sum})
	}
}

// priceAt returns the close of the last session bar at or before minute-of-day m.
func (r *Recorder) priceAt(sym string, m int) float64 {
	bars, _ := r.scn.SessionBars(sym)
	px := 0.0
	for _, b := range bars {
		bt := time.Unix(b.Time, 0).In(r.etz)
		if bt.Hour()*60+bt.Minute() <= m {
			px = b.Close
		} else {
			break
		}
	}
	return px
}

func (r *Recorder) journal(day string, rec map[string]interface{}) {
	rec["t"] = time.Now().In(r.etz).Format("15:04:05")
	f, err := os.OpenFile(filepath.Join(r.dir, day+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(rec); err == nil {
		f.Write(append(b, '\n'))
	}
}

// Report powers /api/moverwatch: today's tracked set + the latest recorded table.
func (r *Recorder) Report() interface{} {
	day := time.Now().In(r.etz).Format("2006-01-02")
	b, err := os.ReadFile(filepath.Join(r.dir, day+".jsonl"))
	if err != nil {
		return map[string]interface{}{"day": day, "marks": 0, "note": "no records yet today"}
	}
	var lastTable interface{}
	marks := 0
	series := map[string][]map[string]interface{}{}
	greens := []map[string]interface{}{}
	for _, ln := range splitLines(b) {
		var rec map[string]interface{}
		if json.Unmarshal(ln, &rec) != nil {
			continue
		}
		switch rec["type"] {
		case "table":
			marks++
			lastTable = rec
		case "green_new":
			greens = append(greens, rec)
		case "px":
			sym, _ := rec["sym"].(string)
			series[sym] = append(series[sym], rec)
		}
	}
	return map[string]interface{}{"day": day, "shadow": true, "marks": marks,
		"greens": greens, "series": series, "last_table": lastTable}
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
