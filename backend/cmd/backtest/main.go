// Command backtest replays historical Alpaca SIP 1-minute bars through the SAME strategy
// detectors the live signal engine uses, simulating bracket execution under the risk
// limits. Read-only: it fetches market data with the configured keys and never places any
// order anywhere. Fetched bars are cached on disk (data/btcache/) so repeat runs and
// parameter sweeps don't refetch.
//
// Modes (run from backend/):
//
//	go run ./cmd/backtest -days 21                     # standard run, all strategies
//	go run ./cmd/backtest -days 63 -sweep              # momentum param sweep: tune on the
//	                                                   # older 2/3 (IS), validate the top
//	                                                   # combos on the held-out 1/3 (OOS)
//	go run ./cmd/backtest -days 21 -strategies momentum_cont -mstop 0.3 -mtarget 0.4 -mhold 90
package main

import (
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/config"
	"live-optimus/backend/internal/risk"
	"live-optimus/backend/internal/signals"
)

// btData is the cached fetch payload.
type btData struct {
	Minute map[string][]signals.Bar
	Daily  map[string][]signals.Bar
}

func main() {
	days := flag.Int("days", 10, "trading days to replay (approximate; whatever bars exist in the window)")
	universePath := flag.String("universe", "", "universe file (default: QUANT_UNIVERSE.json auto-discovery)")
	strats := flag.String("strategies", "", "comma-separated strategy filter (default: all)")
	perPos := flag.Float64("perpos", 2000, "per-position notional target (USD)")
	maxRisk := flag.Float64("risk", 40, "max risk per trade (USD)")
	maxConc := flag.Int("conc", 3, "max concurrent positions")
	lossCap := flag.Float64("losscap", 150, "daily loss cap (USD)")
	slip := flag.Float64("slip", 3, "slippage per side (bps) on market entries/stop exits")
	mstop := flag.Float64("mstop", 0, "momentum_cont stop (ATRs); 0 = default 0.35")
	mtarget := flag.Float64("mtarget", 0, "momentum_cont target (ATRs); 0 = default 0.50")
	mhold := flag.Int("mhold", 0, "momentum_cont max hold (minutes); 0 = until EOD")
	sweep := flag.Bool("sweep", false, "momentum param sweep with an in-sample/out-of-sample split")
	dayFrom := flag.String("from", "", "inclusive ET start date for the simulation (2006-01-02)")
	dayTo := flag.String("to", "", "inclusive ET end date for the simulation")
	dataset := flag.String("dataset", "", "write an ML training JSONL here: one row per signal with features + counterfactual outcome")
	mlgate := flag.Bool("mlgate", false, "enable the walk-forward expected-R ML gate (retrains daily on prior days only)")
	mlmargin := flag.Float64("mlmargin", 0.03, "ML gate: minimum predicted R to take a trade")
	mlpred := flag.String("mlpred", "", "gate entries from an external predictions JSONL (strategy/symbol/time/pred_r), e.g. the LightGBM trainer's output")
	regime := flag.Bool("regime", false, "market-posture brake: no new entries when QQQ's prior close is below its 20-day MA")
	mltopq := flag.Float64("mltopq", 0, "with -mlpred: accept only scores >= this quantile of the strategy's PRIOR-day scores (e.g. 0.70)")
	tod := flag.Bool("tod", false, "block (strategy, half-hour) buckets with proven negative expectancy (online, walk-forward)")
	router := flag.Bool("router", false, "block (strategy, market-state) pairs with proven negative expectancy (online, walk-forward)")
	passive := flag.Bool("passive", false, "passive limit entries at the signal price (5-min rest) instead of market fills")
	throttle := flag.Bool("throttle", false, "half-size strategies whose realized-R EWMA is negative")
	minEntry := flag.Int("minentry", 0, "block entries before this session minute (65 = no entries before 10:35 ET)")
	open30 := flag.Bool("open30", false, "analysis mode: does the first-30-min move predict the rest of the day? (no backtest)")
	noCache := flag.Bool("nocache", false, "bypass the on-disk bar cache")
	chunkDays := flag.Int("chunkdays", 0, "fetch the minute-bar window in consecutive chunks of this many calendar days (avoids Alpaca rate-limit stalls on long windows); 0 = single fetch (default)")
	sectorLag := flag.Bool("sectorlag", false, "P2.1: merge sector_ret_15m/peer_gap_15m into every signal's features (research-only, off by default)")
	ensemble := flag.Bool("ensemble", false, "P2.2: gate entries by 3-model agreement (needs -predreg/-predclf/-predrank); mutually exclusive with -mlpred/-mlgate")
	predReg := flag.String("predreg", "", "P2.2: reg model's predictions JSONL (LightGBM regressor)")
	predClf := flag.String("predclf", "", "P2.2: clf model's predictions JSONL (LightGBM classifier)")
	predRank := flag.String("predrank", "", "P2.2: rank model's predictions JSONL (LightGBM ranker)")
	ensembleClfMargin := flag.Float64("ensembleclfmargin", 0.03, "P2.2: minimum clf expected R to pass its leg")
	ensembleRankQ := flag.Float64("ensemblerankq", 0.70, "P2.2: rank leg must clear this quantile of the strategy's PRIOR-day rank scores")
	outPath := flag.String("out", "", "write full JSON result here (default: data/backtests/<ts>.json)")
	flag.Parse()

	candidates := []string{*universePath, "../QUANT_UNIVERSE.json", "QUANT_UNIVERSE.json"}
	uni, err := signals.LoadUniverse(candidates...)
	if err != nil {
		log.Fatalf("universe: %v", err)
	}
	log.Printf("universe: %d tradable symbols + %d context (%s)", len(uni.Symbols()), len(uni.Context()), strings.Join(uni.Context(), ","))

	data := loadBars(uni, *days, *chunkDays, *noCache)

	if *open30 {
		analyzeOpen30(uni, data.Minute)
		return
	}

	limits := risk.Limits{
		DailyLossCapUSD:    *lossCap,
		MaxRiskPerTradeUSD: *maxRisk,
		MaxPositionUSD:     *perPos,
		MaxConcurrent:      *maxConc,
	}

	if *sweep {
		runSweep(uni, data, limits, *slip)
		return
	}

	btCfg := signals.BTConfig{
		Limits:      limits,
		SlippageBps: *slip,
		Strategies:  strategiesWith(*mstop, *mtarget, *mhold),
		DayFrom:      *dayFrom,
		DayTo:        *dayTo,
		DatasetPath:   *dataset,
		MLGate:        *mlgate,
		MLGateMargin:  *mlmargin,
		RegimeFilter:  *regime,
		MLTopQuantile:  *mltopq,
		TODFilter:      *tod,
		RegimeRouter:   *router,
		PassiveEntry:   *passive,
		Throttle:       *throttle,
		MinEntryMinute: *minEntry,
		SectorLeadLag:  *sectorLag,
		EnsembleAgreement:    *ensemble,
		EnsembleClfMargin:    *ensembleClfMargin,
		EnsembleRankQuantile: *ensembleRankQ,
	}
	if *mlpred != "" {
		preds, err := loadPredictions(*mlpred)
		if err != nil {
			log.Fatalf("mlpred: %v", err)
		}
		btCfg.Predictions = preds
		log.Printf("loaded %d external predictions from %s", len(preds), *mlpred)
	}
	if *ensemble {
		reg, err := loadPredictions(*predReg)
		if err != nil {
			log.Fatalf("predreg: %v", err)
		}
		clf, err := loadPredictions(*predClf)
		if err != nil {
			log.Fatalf("predclf: %v", err)
		}
		rank, err := loadPredictions(*predRank)
		if err != nil {
			log.Fatalf("predrank: %v", err)
		}
		btCfg.PredictionsReg, btCfg.PredictionsClf, btCfg.PredictionsRank = reg, clf, rank
		log.Printf("ensemble: loaded reg=%d clf=%d rank=%d predictions", len(reg), len(clf), len(rank))
	}
	if *strats != "" {
		btCfg.OnlyStrats = strings.Split(*strats, ",")
	}
	res := signals.RunBacktest(uni, data.Minute, data.Daily, btCfg)
	fmt.Println(res.Report())

	out := *outPath
	if out == "" {
		_ = os.MkdirAll(filepath.Join("data", "backtests"), 0o755)
		out = filepath.Join("data", "backtests", time.Now().Format("20060102-150405")+".json")
	}
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		if err := os.WriteFile(out, b, 0o644); err == nil {
			log.Printf("full result written to %s", out)
		}
	}
}

// strategiesWith returns the full strategy set with the momentum params applied.
func strategiesWith(mstop, mtarget float64, mhold int) []signals.Strategy {
	return []signals.Strategy{
		signals.ORBBreakout{},
		signals.VWAPReclaim{},
		signals.MomentumContinuation{StopATR: mstop, TargetATR: mtarget, MaxHoldMin: mhold},
		signals.DipBounce{},
		signals.RelativeStrength{},
		signals.FirstHourReversal{},
	}
}

// runSweep tunes momentum_cont on the OLDER ~2/3 of the window (in-sample) and validates
// the top combos on the held-out final ~1/3 (out-of-sample) — data the tuning never saw.
func runSweep(uni *signals.Universe, data btData, limits risk.Limits, slip float64) {
	days := tradingDays(data.Minute)
	if len(days) < 9 {
		log.Fatalf("sweep needs more history (%d trading days loaded)", len(days))
	}
	split := days[len(days)-len(days)/3] // first OOS day
	log.Printf("sweep window: %s → %s | in-sample through %s, out-of-sample from %s (%d IS / %d OOS days)",
		days[0], days[len(days)-1], days[len(days)-len(days)/3-1], split, len(days)-len(days)/3, len(days)/3)

	type combo struct {
		stop, target float64
		hold         int
	}
	var combos []combo
	for _, s := range []float64{0.25, 0.35} {
		for _, t := range []float64{0.35, 0.50} {
			for _, h := range []int{0, 60, 120} {
				combos = append(combos, combo{s, t, h})
			}
		}
	}

	run := func(c combo, from, to string) *signals.BTResult {
		return signals.RunBacktest(uni, data.Minute, data.Daily, signals.BTConfig{
			Limits:      limits,
			SlippageBps: slip,
			Strategies:  []signals.Strategy{signals.MomentumContinuation{StopATR: c.stop, TargetATR: c.target, MaxHoldMin: c.hold}},
			DayFrom:     from,
			DayTo:       to,
		})
	}

	type scored struct {
		c   combo
		res *signals.BTResult
	}
	var rows []scored
	fmt.Printf("\n──── IN-SAMPLE (momentum_cont only) ────\n")
	fmt.Printf("%-6s %-7s %-5s %7s %7s %9s %9s %8s %7s\n", "stop", "target", "hold", "trades", "hit%", "totalP&L", "avg/day", "maxDD", "avgR")
	for _, c := range combos {
		res := run(c, "", prevDay(days, split))
		s := res.PerStrategy["momentum_cont"]
		fmt.Printf("%-6.2f %-7.2f %-5d %7d %6.1f%% %9.2f %9.2f %8.2f %7.2f\n",
			c.stop, c.target, c.hold, s.Trades, s.HitRate, res.TotalPNL, res.AvgDayPNL, res.MaxDrawdown, s.AvgR)
		rows = append(rows, scored{c, res})
	}

	// Rank by risk-adjusted quality: P&L per day penalized by drawdown; demand real activity.
	sort.Slice(rows, func(i, j int) bool { return score(rows[i].res) > score(rows[j].res) })
	top := rows
	if len(top) > 3 {
		top = top[:3]
	}

	fmt.Printf("\n──── OUT-OF-SAMPLE VALIDATION (top 3 by IS risk-adjusted score) ────\n")
	fmt.Printf("%-6s %-7s %-5s %7s %7s %9s %9s %8s %7s\n", "stop", "target", "hold", "trades", "hit%", "totalP&L", "avg/day", "maxDD", "avgR")
	for _, r := range top {
		res := run(r.c, split, "")
		s := res.PerStrategy["momentum_cont"]
		fmt.Printf("%-6.2f %-7.2f %-5d %7d %6.1f%% %9.2f %9.2f %8.2f %7.2f\n",
			r.c.stop, r.c.target, r.c.hold, s.Trades, s.HitRate, res.TotalPNL, res.AvgDayPNL, res.MaxDrawdown, s.AvgR)
	}
	fmt.Println("\nA combo is trustworthy only if its OOS row is also positive — an IS winner that")
	fmt.Println("dies OOS was curve-fit. Pick from the OOS survivors, never from the IS table alone.")
}

// score ranks a run by average daily P&L penalized by drawdown; inactive configs rank last.
func score(r *signals.BTResult) float64 {
	trades := 0
	for _, s := range r.PerStrategy {
		trades += s.Trades
	}
	if trades < 10 {
		return -1e9
	}
	return r.AvgDayPNL - 0.05*r.MaxDrawdown
}

// tradingDays lists the sorted ET dates present in the minute data.
func tradingDays(minute map[string][]signals.Bar) []string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	set := map[string]bool{}
	for _, bars := range minute {
		for _, b := range bars {
			n := time.Unix(b.Time, 0).In(loc)
			if n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
				continue
			}
			set[n.Format("2006-01-02")] = true
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// prevDay returns the trading day immediately before `day`.
func prevDay(days []string, day string) string {
	prev := ""
	for _, d := range days {
		if d >= day {
			break
		}
		prev = d
	}
	return prev
}

// loadBars fetches (or loads from cache) the daily + minute bars for the window. When
// chunkDays > 0 the minute-bar window is split into consecutive chunkDays-day pieces —
// each fetched and cached independently — instead of one large request, since a single
// ~357-day fetch stalls on Alpaca rate limits.
func loadBars(uni *signals.Universe, days, chunkDays int, noCache bool) btData {
	end := time.Now()
	start := end.AddDate(0, 0, -(days*7/5 + 5))

	if chunkDays > 0 {
		return loadBarsChunked(uni, days, chunkDays, start, end, noCache)
	}

	cachePath := filepath.Join("data", "btcache",
		fmt.Sprintf("%s_%s_%dsyms.gob", start.Format("20060102"), end.Format("20060102"), len(uni.All())))

	if !noCache {
		if f, err := os.Open(cachePath); err == nil {
			defer f.Close()
			var data btData
			if err := gob.NewDecoder(f).Decode(&data); err == nil {
				log.Printf("loaded bars from cache %s", cachePath)
				return data
			}
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	client := alpaca.New(cfg)
	symbols := uni.All()

	log.Printf("fetching daily bars (%d symbols, ~%d days of context)...", len(symbols), days+30)
	daily, err := client.GetMultiDailyBars(symbols, days+30)
	if err != nil {
		log.Fatalf("daily bars: %v", err)
	}
	log.Printf("fetching 1-minute bars %s → %s (heavy call — cached for repeat runs)...",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	minute, err := client.GetMultiIntradayBars(symbols, start, end)
	if err != nil {
		log.Fatalf("minute bars: %v", err)
	}
	data := btData{Minute: toBars(minute), Daily: toBars(daily)}
	var n int
	for _, bs := range data.Minute {
		n += len(bs)
	}
	log.Printf("loaded %d minute bars across %d symbols", n, len(data.Minute))

	if !noCache {
		_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
		if f, err := os.Create(cachePath); err == nil {
			defer f.Close()
			if gob.NewEncoder(f).Encode(&data) == nil {
				log.Printf("cached bars to %s", cachePath)
			}
		}
	}
	return data
}

// loadBarsChunked is loadBars' chunked-fetch path (see loadBars doc). Each chunk's cache
// file uses the same "<start>_<end>_<n>syms.gob" naming as the unchunked path — the chunk
// bounds make the name unique — so chunk caches never collide with the whole-window cache.
func loadBarsChunked(uni *signals.Universe, days, chunkDays int, start, end time.Time, noCache bool) btData {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	client := alpaca.New(cfg)
	symbols := uni.All()

	log.Printf("fetching daily bars (%d symbols, ~%d days of context)...", len(symbols), days+30)
	daily, err := client.GetMultiDailyBars(symbols, days+30)
	if err != nil {
		log.Fatalf("daily bars: %v", err)
	}

	minute := map[string][]signals.Bar{}
	chunkN := 0
	for cs := start; cs.Before(end); cs = cs.AddDate(0, 0, chunkDays) {
		ce := cs.AddDate(0, 0, chunkDays)
		if ce.After(end) {
			ce = end
		}
		chunkN++
		chunkPath := filepath.Join("data", "btcache",
			fmt.Sprintf("%s_%s_%dsyms.gob", cs.Format("20060102"), ce.Format("20060102"), len(symbols)))

		var chunkBars map[string][]signals.Bar
		if !noCache {
			if f, err := os.Open(chunkPath); err == nil {
				var m map[string][]signals.Bar
				decErr := gob.NewDecoder(f).Decode(&m)
				f.Close()
				if decErr == nil {
					chunkBars = m
					log.Printf("chunk %d/%s→%s: loaded from cache %s", chunkN, cs.Format("2006-01-02"), ce.Format("2006-01-02"), chunkPath)
				}
			}
		}
		if chunkBars == nil {
			log.Printf("chunk %d: fetching %s → %s...", chunkN, cs.Format("2006-01-02"), ce.Format("2006-01-02"))
			raw, err := client.GetMultiIntradayBars(symbols, cs, ce)
			if err != nil {
				log.Fatalf("minute bars chunk %s-%s: %v", cs.Format("2006-01-02"), ce.Format("2006-01-02"), err)
			}
			chunkBars = toBars(raw)
			if !noCache {
				_ = os.MkdirAll(filepath.Dir(chunkPath), 0o755)
				if f, err := os.Create(chunkPath); err == nil {
					if gob.NewEncoder(f).Encode(&chunkBars) == nil {
						log.Printf("chunk %d: cached to %s", chunkN, chunkPath)
					}
					f.Close()
				}
			}
		}

		var n int
		for sym, bs := range chunkBars {
			minute[sym] = append(minute[sym], bs...)
			n += len(bs)
		}
		log.Printf("chunk %d: %d bars across %d symbols", chunkN, n, len(chunkBars))
	}

	for sym := range minute {
		bs := minute[sym]
		sort.Slice(bs, func(i, j int) bool { return bs[i].Time < bs[j].Time })
		minute[sym] = bs
	}

	data := btData{Minute: minute, Daily: toBars(daily)}
	var n int
	for _, bs := range data.Minute {
		n += len(bs)
	}
	log.Printf("loaded %d minute bars across %d symbols (%d chunks of %d days)", n, len(data.Minute), chunkN, chunkDays)
	return data
}

// analyzeOpen30 tests the operator's hypothesis "a stock that rises in the first 30
// minutes falls for the rest of the day, and vice versa": for every (symbol, day) it
// computes r1 = return from the 09:30 open to 10:00 ET and r2 = return from 10:00 to the
// 15:55 close, then reports the correlation and what actually happened after the biggest
// early risers/fallers. Pure measurement — no trading logic.
func analyzeOpen30(uni *signals.Universe, minute map[string][]signals.Bar) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	var all []open30Obs
	for _, sym := range uni.Symbols() {
		byDay := map[string][]signals.Bar{}
		for _, b := range minute[sym] {
			n := time.Unix(b.Time, 0).In(loc)
			if n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
				continue
			}
			o := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, loc).Unix()
			if b.Time < o || b.Time >= o+390*60 {
				continue
			}
			byDay[n.Format("2006-01-02")] = append(byDay[n.Format("2006-01-02")], b)
		}
		for _, bars := range byDay {
			sort.Slice(bars, func(i, j int) bool { return bars[i].Time < bars[j].Time })
			open := bars[0].Open
			day0 := time.Unix(bars[0].Time, 0).In(loc)
			t10 := time.Date(day0.Year(), day0.Month(), day0.Day(), 10, 0, 0, 0, loc).Unix()
			t1555 := time.Date(day0.Year(), day0.Month(), day0.Day(), 15, 55, 0, 0, loc).Unix()
			var px10, pxEnd float64
			for _, b := range bars {
				if b.Time <= t10 {
					px10 = b.Close
				}
				if b.Time <= t1555 {
					pxEnd = b.Close
				}
			}
			if open <= 0 || px10 <= 0 || pxEnd <= 0 || len(bars) < 100 {
				continue
			}
			all = append(all, open30Obs{r1: (px10 - open) / open * 100, r2: (pxEnd - px10) / px10 * 100})
		}
	}
	if len(all) < 100 {
		fmt.Printf("not enough (symbol, day) observations: %d\n", len(all))
		return
	}
	// Pearson correlation r1 vs r2.
	var m1, m2 float64
	for _, o := range all {
		m1 += o.r1
		m2 += o.r2
	}
	m1 /= float64(len(all))
	m2 /= float64(len(all))
	var cov, v1, v2 float64
	for _, o := range all {
		cov += (o.r1 - m1) * (o.r2 - m2)
		v1 += (o.r1 - m1) * (o.r1 - m1)
		v2 += (o.r2 - m2) * (o.r2 - m2)
	}
	corr := cov / (math.Sqrt(v1) * math.Sqrt(v2))

	sort.Slice(all, func(i, j int) bool { return all[i].r1 < all[j].r1 })
	dec := len(all) / 10
	bucket := func(list []open30Obs) (meanR2 float64, fellPct float64) {
		fell := 0
		for _, o := range list {
			meanR2 += o.r2
			if o.r2 < 0 {
				fell++
			}
		}
		return meanR2 / float64(len(list)), float64(fell) / float64(len(list)) * 100
	}
	loMean, loFell := bucket(all[:dec])          // biggest early FALLERS
	hiMean, hiFell := bucket(all[len(all)-dec:]) // biggest early RISERS
	midMean, midFell := bucket(all[4*dec : 6*dec])

	fmt.Printf("\n════ Does the first 30 minutes predict the rest of the day? ════\n")
	fmt.Printf("observations: %d (symbol, day) pairs · correlation(first30, rest-of-day): %+.3f\n\n", len(all), corr)
	fmt.Printf("%-34s %10s %14s\n", "group (by first-30-min move)", "avg rest", "% that fell")
	fmt.Printf("%-34s %+9.2f%% %13.0f%%\n", fmt.Sprintf("top decile risers   (avg %+.1f%%)", avgR1(all[len(all)-dec:])), hiMean, hiFell)
	fmt.Printf("%-34s %+9.2f%% %13.0f%%\n", "middle (flat opens)", midMean, midFell)
	fmt.Printf("%-34s %+9.2f%% %13.0f%%\n", fmt.Sprintf("bottom decile fallers (avg %+.1f%%)", avgR1(all[:dec])), loMean, loFell)
	fmt.Printf("\nReading: a correlation near 0 = no reliable relationship; 'always falls' would\nshow up as a strongly negative correlation and a fell%% far above 50%%.\n")
}

// open30Obs is one (symbol, day) observation for the first-30-minutes analysis.
type open30Obs struct{ r1, r2 float64 }

func avgR1(list []open30Obs) float64 {
	var s float64
	for _, o := range list {
		s += o.r1
	}
	return s / float64(len(list))
}

// loadPredictions reads a predictions JSONL (from ml/train_gate.py) into the gate map.
func loadPredictions(path string) (map[string]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			Strategy string  `json:"strategy"`
			Symbol   string  `json:"symbol"`
			Time     int64   `json:"time"`
			PredR    float64 `json:"pred_r"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s|%s|%d", row.Strategy, row.Symbol, row.Time)] = row.PredR
	}
	return out, nil
}

// toBars converts the alpaca DTO bars to the signals package's bar type.
func toBars(in map[string][]alpaca.HistBar) map[string][]signals.Bar {
	out := make(map[string][]signals.Bar, len(in))
	for sym, bars := range in {
		bs := make([]signals.Bar, 0, len(bars))
		for _, b := range bars {
			bs = append(bs, signals.Bar{
				Time: b.Time.Unix(), Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume,
			})
		}
		out[sym] = bs
	}
	return out
}
