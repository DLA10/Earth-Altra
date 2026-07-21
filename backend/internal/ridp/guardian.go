// Guardian is the SHADOW desk-P&L overseer for RIDP (operator + assistant designs,
// 2026-07-21). PURE OBSERVER: its inputs are broker Account()/Positions() reads, candle
// snapshots, and a tail of trades.jsonl — it holds no code path that can place, cancel,
// or modify an order, so it cannot touch the live desk even by bug.
//
// It evaluates ALL candidate manager rules simultaneously as counterfactuals and
// journals to data/ridp/guardian_<day>.jsonl:
//
//	sample      once/minute {pnl, peak} — the desk curve as the operator sees it
//	desk_stop   first trigger/day per (pmin, giveback) config  [operator's fixed rule]
//	ratchet     first floor breach/day per arm config          [50%-of-peak rising floor]
//	pos_lock    per-position profit-lock triggers (arm, giveback grid)
//	cascade     >=3 reverter stop-outs within 2 minutes (correlated knife event)
//	would_bench second same-day losing trade on a symbol
//	eod         summary: actual close P&L, peak, giveback, per-config would-have P&L
//
// Friday's decision table is built from these records. Nothing here trades.
package ridp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/quant"
)

var (
	gDeskPmins = []float64{15, 25}
	gDeskGBs   = []float64{10, 15, 20}
	gRatchArms = []float64{20, 40}
	gLockArms  = []float64{10, 15, 20}
	gLockGBs   = []float64{3, 5, 7}
)

type Guardian struct {
	broker *quant.Broker
	engine *candles.Engine
	etz    *time.Location
	dir    string

	day       string
	f         *os.File
	peak      float64
	stopTrig  map[string]float64 // fixed desk-stop: cfg -> pnl at trigger
	ratchTrig map[string]float64 // ratchet: arm -> pnl at breach
	ratchFlr  map[string]float64 // ratchet state: arm -> current floor (armed only)
	posPeak   map[string]float64 // sym -> peak unrealized $ this holding episode
	lockFired map[string]bool    // sym|arm|gb -> fired this episode
	benchCnt  map[string]int     // sym -> losing closes today
	benchLog  map[string]bool
	stopTimes []time.Time // recent reverter stop-out close times
	lastCasc  time.Time
	tailOff   int64
	lastMin   int
	eodDone   bool
}

func NewGuardian(b *quant.Broker, eng *candles.Engine, etz *time.Location, dataDir string) *Guardian {
	return &Guardian{broker: b, engine: eng, etz: etz, dir: filepath.Join(dataDir, "ridp")}
}

func (g *Guardian) Start(ctx context.Context) {
	if g == nil || g.broker == nil || !g.broker.Enabled() {
		return
	}
	// start the journal tail at EOF — only trades closed from now on matter
	if fi, err := os.Stat(filepath.Join(g.dir, "trades.jsonl")); err == nil {
		g.tailOff = fi.Size()
	}
	g.resetDay(time.Now().In(g.etz))
	log.Printf("ridp: shadow GUARDIAN on — log-only, %d desk-stop + %d ratchet + %d lock configs → data/ridp/guardian_<day>.jsonl",
		len(gDeskPmins)*len(gDeskGBs), len(gRatchArms), len(gLockArms)*len(gLockGBs))
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if g.f != nil {
					g.f.Close()
				}
				return
			case <-t.C:
				g.cycle()
			}
		}
	}()
}

func (g *Guardian) resetDay(now time.Time) {
	day := now.Format("2006-01-02")
	if day == g.day {
		return
	}
	if g.f != nil {
		g.f.Close()
	}
	g.day = day
	g.peak = 0
	g.stopTrig = map[string]float64{}
	g.ratchTrig = map[string]float64{}
	g.ratchFlr = map[string]float64{}
	g.posPeak = map[string]float64{}
	g.lockFired = map[string]bool{}
	g.benchCnt = map[string]int{}
	g.benchLog = map[string]bool{}
	g.stopTimes = nil
	g.lastMin = -1
	g.eodDone = false
	_ = os.MkdirAll(g.dir, 0755)
	f, err := os.OpenFile(filepath.Join(g.dir, "guardian_"+day+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		g.f = f
	}
}

func (g *Guardian) journal(rec map[string]interface{}) {
	if g.f == nil {
		return
	}
	rec["t"] = time.Now().In(g.etz).Format("15:04:05")
	b, err := json.Marshal(rec)
	if err == nil {
		g.f.Write(append(b, '\n'))
	}
}

func (g *Guardian) cycle() {
	now := time.Now().In(g.etz)
	g.resetDay(now)
	wd := now.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return
	}
	hm := now.Hour()*60 + now.Minute()
	if hm < 9*60+25 || hm > 16*60+10 {
		return
	}

	acct, err := g.broker.Account()
	if err != nil {
		return // transient; never act (or log noise) on a failed read
	}
	pnl := acct.DayPnL()
	if pnl > g.peak {
		g.peak = pnl
	}

	// once-a-minute curve sample
	if hm != g.lastMin {
		g.lastMin = hm
		g.journal(map[string]interface{}{"type": "sample", "pnl": round2g(pnl), "peak": round2g(g.peak)})
	}

	// operator's fixed desk-stop grid
	for _, pmin := range gDeskPmins {
		for _, gb := range gDeskGBs {
			key := fmt.Sprintf("%.0f/%.0f", pmin, gb)
			if _, done := g.stopTrig[key]; done {
				continue
			}
			if g.peak >= pmin && pnl <= g.peak-gb {
				g.stopTrig[key] = pnl
				g.journal(map[string]interface{}{"type": "desk_stop", "cfg": key,
					"pnl": round2g(pnl), "peak": round2g(g.peak),
					"note": "would flatten + halt entries for the day"})
			}
		}
	}

	// ratcheting floor: arms at +arm$, floor = 50% of peak, rises only
	for _, arm := range gRatchArms {
		key := fmt.Sprintf("%.0f", arm)
		if _, done := g.ratchTrig[key]; done {
			continue
		}
		flr, armed := g.ratchFlr[key]
		if !armed {
			if pnl >= arm {
				g.ratchFlr[key] = g.peak * 0.5
			}
			continue
		}
		if nf := g.peak * 0.5; nf > flr {
			flr = nf
			g.ratchFlr[key] = nf
		}
		if pnl < flr {
			g.ratchTrig[key] = pnl
			g.journal(map[string]interface{}{"type": "ratchet", "arm": arm,
				"floor": round2g(flr), "pnl": round2g(pnl), "peak": round2g(g.peak),
				"note": "would flatten + halt; floor was 50% of peak"})
		}
	}

	// per-position profit locks (broker positions marked at last 1-min close)
	positions, perr := g.broker.Positions()
	if perr == nil {
		held := map[string]bool{}
		for _, p := range positions {
			if p.Qty <= 0 || p.AvgEntry <= 0 {
				continue
			}
			held[p.Symbol] = true
			bars := g.engine.Snapshot(p.Symbol, 1)
			if len(bars) == 0 {
				continue
			}
			unreal := (bars[len(bars)-1].Close - p.AvgEntry) * p.Qty
			if unreal > g.posPeak[p.Symbol] {
				g.posPeak[p.Symbol] = unreal
			}
			pk := g.posPeak[p.Symbol]
			for _, arm := range gLockArms {
				for _, gb := range gLockGBs {
					key := fmt.Sprintf("%s|%.0f|%.0f", p.Symbol, arm, gb)
					if g.lockFired[key] || pk < arm || unreal > pk-gb {
						continue
					}
					g.lockFired[key] = true
					g.journal(map[string]interface{}{"type": "pos_lock", "sym": p.Symbol,
						"arm": arm, "gb": gb, "locked": round2g(unreal), "pos_peak": round2g(pk)})
				}
			}
		}
		for sym := range g.posPeak { // holding episode ended → reset that symbol
			if !held[sym] {
				delete(g.posPeak, sym)
				for _, arm := range gLockArms {
					for _, gb := range gLockGBs {
						delete(g.lockFired, fmt.Sprintf("%s|%.0f|%.0f", sym, arm, gb))
					}
				}
			}
		}
	}

	g.tailTrades(now)

	// EOD summary (once, after the desk's 15:55 flatten settles)
	if hm >= 16*60+5 && !g.eodDone {
		g.eodDone = true
		g.journal(map[string]interface{}{"type": "eod", "pnl": round2g(pnl),
			"peak": round2g(g.peak), "giveback": round2g(g.peak - pnl),
			"desk_stop_wouldhave": g.stopTrig, "ratchet_wouldhave": g.ratchTrig})
	}
}

// tailTrades reads newly appended trade closes and evaluates the cascade and
// would-bench detectors. File-based on purpose: zero coupling to the live Manager.
func (g *Guardian) tailTrades(now time.Time) {
	path := filepath.Join(g.dir, "trades.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= g.tailOff {
		if err == nil && fi.Size() < g.tailOff {
			g.tailOff = 0 // file rotated/truncated
		}
		return
	}
	if _, err := f.Seek(g.tailOff, 0); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var tr struct {
			Strategy string  `json:"strategy"`
			Symbol   string  `json:"symbol"`
			PnL      float64 `json:"pnl"`
			Reason   string  `json:"reason"`
		}
		if json.Unmarshal(line, &tr) != nil {
			continue
		}
		if tr.PnL < 0 {
			g.benchCnt[tr.Symbol]++
			if g.benchCnt[tr.Symbol] >= 2 && !g.benchLog[tr.Symbol] {
				g.benchLog[tr.Symbol] = true
				g.journal(map[string]interface{}{"type": "would_bench", "sym": tr.Symbol,
					"losses_today": g.benchCnt[tr.Symbol],
					"note": "2nd losing close — symbol would sit out the rest of the day"})
			}
		}
		low := strings.ToLower(tr.Reason)
		if tr.Strategy == "reverter" && (strings.Contains(low, "stop") || strings.Contains(low, "broke")) {
			g.stopTimes = append(g.stopTimes, now)
			cutoff := now.Add(-2 * time.Minute)
			live := g.stopTimes[:0]
			for _, t := range g.stopTimes {
				if t.After(cutoff) {
					live = append(live, t)
				}
			}
			g.stopTimes = live
			if len(g.stopTimes) >= 3 && now.Sub(g.lastCasc) > 5*time.Minute {
				g.lastCasc = now
				g.journal(map[string]interface{}{"type": "cascade",
					"stops_2min": len(g.stopTimes),
					"note": "correlated knife event — reverter would halt 30 min"})
			}
		}
	}
	if sc.Err() != nil {
		return // partial read — keep the old offset and retry next cycle
	}
	g.tailOff = fi.Size() // clean read to EOF; never re-process these lines
}

func round2g(v float64) float64 {
	if v < 0 {
		return float64(int64(v*100-0.5)) / 100
	}
	return float64(int64(v*100+0.5)) / 100
}
