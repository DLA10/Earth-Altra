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
	noCache := flag.Bool("nocache", false, "bypass the on-disk bar cache")
	outPath := flag.String("out", "", "write full JSON result here (default: data/backtests/<ts>.json)")
	flag.Parse()

	candidates := []string{*universePath, "../QUANT_UNIVERSE.json", "QUANT_UNIVERSE.json"}
	uni, err := signals.LoadUniverse(candidates...)
	if err != nil {
		log.Fatalf("universe: %v", err)
	}
	log.Printf("universe: %d tradable symbols + %d context (%s)", len(uni.Symbols()), len(uni.Context()), strings.Join(uni.Context(), ","))

	data := loadBars(uni, *days, *noCache)

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
		DatasetPath:  *dataset,
		MLGate:       *mlgate,
		MLGateMargin: *mlmargin,
		RegimeFilter: *regime,
	}
	if *mlpred != "" {
		preds, err := loadPredictions(*mlpred)
		if err != nil {
			log.Fatalf("mlpred: %v", err)
		}
		btCfg.Predictions = preds
		log.Printf("loaded %d external predictions from %s", len(preds), *mlpred)
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

// loadBars fetches (or loads from cache) the daily + minute bars for the window.
func loadBars(uni *signals.Universe, days int, noCache bool) btData {
	end := time.Now()
	start := end.AddDate(0, 0, -(days*7/5 + 5))
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
