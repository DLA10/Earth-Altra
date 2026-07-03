package signals

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"live-optimus/backend/internal/risk"
)

// cfTrack follows one published signal's counterfactual bracket for the ML dataset.
type cfTrack struct {
	sig       Signal
	day       string
	traded    bool
	gate      int // 0 = no model yet (warmup), 1 = gate accepted, 2 = gate rejected
	predR     float64
	todBucket int // half-hour-of-session index of the signal (for #3 conditioning)
	dayState  int // market state of the signal's day: 1 = risk-on, 0 = risk-off (#4)
}

// runStat is an online mean tracker for the conditioning gates (#3/#4).
type runStat struct {
	n   int
	sum float64
}

func (s *runStat) add(r float64) { s.n++; s.sum += r }
func (s *runStat) blocks() bool  { return s.n >= condMinSamples && s.sum/float64(s.n) < 0 }

// ewmaStat is the sizing throttle's tracker (#6).
type ewmaStat struct {
	n    int
	mean float64
}

func (s *ewmaStat) add(r float64) {
	s.n++
	if s.n == 1 {
		s.mean = r
		return
	}
	s.mean = (1-throttleAlpha)*s.mean + throttleAlpha*r
}
func (s *ewmaStat) halves() bool { return s.n >= condMinSamples && s.mean < 0 }

// BTConfig configures a backtest run. Limits are the same risk guardrails the live-paper
// pipeline will trade under, so backtest results and paper results are comparable.
type BTConfig struct {
	Limits      risk.Limits
	SlippageBps float64  // per side, applied to market entries/stop exits (targets are limits)
	Strategies  []Strategy
	OnlyStrats  []string // optional filter (strategy names); empty = all
	DayFrom     string   // inclusive ET date bound ("2006-01-02"); empty = unbounded
	DayTo       string   // inclusive ET date bound; empty = unbounded
	// DatasetPath, when set, writes one JSONL row per PUBLISHED signal (traded or not)
	// with its full feature snapshot and counterfactual bracket outcome — the ML training
	// set for the Phase-2 p(win) gate, bootstrapped from historical replay.
	DatasetPath string
	// MLGate enables the walk-forward expected-R gate: at the start of each simulated
	// day the per-strategy ridge models retrain on all outcomes resolved on PRIOR days,
	// then that day's entries require predicted R ≥ MLGateMargin. Strategies without
	// enough history yet trade ungated (warmup). Zero lookahead by construction.
	MLGate       bool
	MLGateMargin float64 // default 0.03 expected R
	// Predictions gates entries from externally computed per-signal scores (e.g. the
	// Python LightGBM walk-forward trainer), keyed "strategy|symbol|unixtime" → predicted
	// R. Signals without a score trade ungated (warmup). Mutually exclusive with MLGate.
	Predictions map[string]float64
	// RegimeFilter is the deterministic market-posture brake (the Strategist's
	// stand_down, QUANT_VISION §4): no NEW entries on days where QQQ's prior close is
	// below its 20-day moving average (computed from prior days only — no lookahead).
	// Long-only bounce/momentum strategies have no business fighting a falling tape.
	RegimeFilter bool

	// ---- Tier-1 research mechanisms (RESEARCH_BACKLOG). All rules pre-registered; ----
	// ---- all statistics learned online from outcomes resolved BEFORE the entry.   ----

	// MLTopQuantile (#2, with an external ranking-model predictions file): accept a
	// signal only if its score is at/above this quantile of the SAME strategy's scores
	// from PRIOR days (causal top-K approximation; needs ≥ condMinScores prior scores,
	// else warmup pass-through). Overrides the absolute-margin rule.
	MLTopQuantile float64
	// TODFilter (#3): block a (strategy, half-hour-of-session) bucket once ≥
	// condMinSamples resolved outcomes show a negative mean R for that bucket.
	TODFilter bool
	// RegimeRouter (#4): block a (strategy, market-state) pair — state = QQQ prior
	// close above/below its 20-day MA — once ≥ condMinSamples resolved outcomes show a
	// negative mean R for that pair. Soft routing: other strategies keep trading.
	RegimeRouter bool
	// PassiveEntry (#5): instead of a market fill at signal close + slippage, rest a
	// limit at the signal price for passiveWindowMin minutes; fill (at the limit, no
	// slippage) only if price trades through it, else the trade is missed.
	PassiveEntry bool
	// Throttle (#6): halve position size for a strategy whose EWMA of realized R
	// (α = throttleAlpha, ≥ condMinSamples observations) is negative.
	Throttle bool
	// MinEntryMinute blocks ENTRIES (signals still journal) before this session minute —
	// the operator's "avoid the first N minutes" rule (e.g. 65 = no entries before
	// 10:35 ET). 0 = off.
	MinEntryMinute int

	// SectorLeadLag (P2.1, RESEARCH_BACKLOG #9): merge sector_ret_15m / peer_gap_15m into
	// every published signal's Features, computed from the same-day session bars of its
	// sector peers. Research-only; off by default so existing runs are unaffected.
	SectorLeadLag bool

	// ---- P2.2: ensemble agreement filter (RESEARCH_BACKLOG #10, simplified — this is ----
	// ---- an agreement filter over 3 model families, NOT formal conformal prediction). ----

	// PredictionsReg/Clf/Rank are three parallel walk-forward prediction maps (same
	// "strategy|symbol|unixtime" keying as Predictions), one per model family, used only
	// when EnsembleAgreement is on. All three must have a score for a signal before the
	// gate applies; missing any one is warmup pass-through (ungated), same as Predictions.
	PredictionsReg  map[string]float64
	PredictionsClf  map[string]float64
	PredictionsRank map[string]float64
	// EnsembleAgreement: accept a signal only when PredictionsClf >= EnsembleClfMargin AND
	// PredictionsRank clears its causal per-strategy EnsembleRankQuantile (computed from
	// PRIOR days only, same mechanism as MLTopQuantile) AND PredictionsReg >= 0. Mutually
	// exclusive with MLGate/Predictions in practice (whichever the caller sets up).
	EnsembleAgreement    bool
	EnsembleClfMargin    float64 // default 0.03
	EnsembleRankQuantile float64 // default 0.70
}

// Pre-registered Tier-1 constants — fixed before any experiment ran; never swept.
const (
	condMinSamples   = 30   // outcomes required before a conditioning bucket may block
	condMinScores    = 100  // prior scores required before the top-quantile gate applies
	passiveWindowMin = 5    // minutes a passive limit rests before it is cancelled
	throttleAlpha    = 0.05 // EWMA weight for the sizing throttle
)

// DatasetRow is one ML training example: a signal's features and what its bracket did.
type DatasetRow struct {
	Day         string             `json:"day"`
	Strategy    string             `json:"strategy"`
	Symbol      string             `json:"symbol"`
	Sector      string             `json:"sector,omitempty"`
	Time        int64              `json:"time"`
	Entry       float64            `json:"entry"`
	Stop        float64            `json:"stop"`
	Target      float64            `json:"target"`
	Features    map[string]float64 `json:"features"`
	Outcome     string             `json:"outcome"` // target | stop | time | eod
	ExitPrice   float64            `json:"exit_price"`
	RMultiple   float64            `json:"r_multiple"`
	MinutesHeld int64              `json:"minutes_held"`
	Traded      bool               `json:"traded"` // whether the sim actually took it
}

// BTTrade is one simulated round trip.
type BTTrade struct {
	Strategy   string  `json:"strategy"`
	Symbol     string  `json:"symbol"`
	Day        string  `json:"day"`
	EntryTime  int64   `json:"entry_time"`
	ExitTime   int64   `json:"exit_time"`
	Entry      float64 `json:"entry"`
	Exit       float64 `json:"exit"`
	Stop       float64 `json:"stop"`
	Target     float64 `json:"target"`
	Qty        float64 `json:"qty"`
	PNL        float64 `json:"pnl"`
	R          float64 `json:"r_multiple"`
	ExitReason string  `json:"exit_reason"` // stop | target | eod
}

// StratStats aggregates one strategy's performance.
type StratStats struct {
	Signals    int     `json:"signals"`
	Trades     int     `json:"trades"`
	Wins       int     `json:"wins"`
	Losses     int     `json:"losses"`
	Timeouts   int     `json:"timeouts"`   // EOD-flatten exits
	TimeExits  int     `json:"time_exits"` // max-hold exits
	HitRate    float64 `json:"hit_rate_pct"`
	TotalPNL   float64 `json:"total_pnl"`
	AvgPNL     float64 `json:"avg_pnl"`
	AvgWin     float64 `json:"avg_win"`
	AvgLoss    float64 `json:"avg_loss"`
	AvgR       float64 `json:"avg_r"`
	AvgMinutes float64 `json:"avg_minutes_held"`

	// Per-strategy ML-gate selectivity (counterfactual R of accepted vs rejected).
	GateAccepted     int     `json:"gate_accepted,omitempty"`
	GateRejected     int     `json:"gate_rejected,omitempty"`
	GateAcceptedAvgR float64 `json:"gate_accepted_avg_r,omitempty"`
	GateRejectedAvgR float64 `json:"gate_rejected_avg_r,omitempty"`
}

// DayPNL is one day's result.
type DayPNL struct {
	Day    string  `json:"day"`
	Trades int     `json:"trades"`
	PNL    float64 `json:"pnl"`
	Halted bool    `json:"halted"` // daily loss cap tripped
}

// BTResult is the full backtest report.
type BTResult struct {
	Days        []DayPNL               `json:"days"`
	PerStrategy map[string]*StratStats `json:"per_strategy"`
	Trades      []BTTrade              `json:"trades"`
	TotalPNL    float64                `json:"total_pnl"`
	AvgDayPNL   float64                `json:"avg_day_pnl"`
	MaxDrawdown float64                `json:"max_drawdown"`
	SkippedRisk int                    `json:"skipped_risk"` // signals skipped by risk caps
	SkippedSize int                    `json:"skipped_size"` // signals unfundable at 1 share

	// ML-gate lift accounting (set when MLGate is on): counterfactual avg R of the
	// signals the gate accepted vs rejected — the direct measure of its selectivity.
	GateOn           bool    `json:"gate_on,omitempty"`
	GateWarmup       int     `json:"gate_warmup,omitempty"`   // signals seen before a model existed
	GateAccepted     int     `json:"gate_accepted,omitempty"` // signals scored and passed
	GateRejected     int     `json:"gate_rejected,omitempty"` // signals scored and blocked
	GateAcceptedAvgR float64 `json:"gate_accepted_avg_r,omitempty"`
	GateRejectedAvgR float64 `json:"gate_rejected_avg_r,omitempty"`

	// Regime-filter accounting.
	RegimeOn      bool `json:"regime_on,omitempty"`
	StandDownDays int  `json:"stand_down_days,omitempty"`
	SkippedRegime int  `json:"skipped_regime,omitempty"` // signals blocked on stand-down days

	// Tier-1 mechanism accounting.
	SkippedEarly    int `json:"skipped_early,omitempty"`    // min-entry-minute blocked entries
	SkippedTOD      int `json:"skipped_tod,omitempty"`      // #3 blocked entries
	SkippedRouter   int `json:"skipped_router,omitempty"`   // #4 blocked entries
	PassiveAttempts int `json:"passive_attempts,omitempty"` // #5 limit orders rested
	PassiveFills    int `json:"passive_fills,omitempty"`    // #5 filled
	PassiveMisses   int `json:"passive_misses,omitempty"`   // #5 expired unfilled
	ThrottledHalf   int `json:"throttled_half,omitempty"`   // #6 half-sized entries
}

// btPosition is one open simulated position.
type btPosition struct {
	sig   Signal
	entry float64
	qty   float64
}

// pendingEntry is a passive limit order resting at the signal price (#5).
type pendingEntry struct {
	sig     Signal
	cf      *cfTrack
	expires int64
}

// RunBacktest replays historical 1-minute bars through the SAME detectors the live engine
// uses, simulating bracket execution under the risk limits. minuteBars/dailyBars are
// keyed by symbol; dailyBars must extend ~20 trading days before the first minute-bar day
// so ATR/avg-volume context exists from day one. qqq/spy come from minuteBars too.
func RunBacktest(uni *Universe, minuteBars, dailyBars map[string][]Bar, cfg BTConfig) *BTResult {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	strats := cfg.Strategies
	if len(strats) == 0 {
		strats = DefaultStrategies()
	}
	if len(cfg.OnlyStrats) > 0 {
		want := map[string]bool{}
		for _, s := range cfg.OnlyStrats {
			want[strings.TrimSpace(strings.ToLower(s))] = true
		}
		var keep []Strategy
		for _, st := range strats {
			if want[st.Name()] {
				keep = append(keep, st)
			}
		}
		strats = keep
	}

	dayOf := func(t int64) string { return time.Unix(t, 0).In(loc).Format("2006-01-02") }

	// Regular-session minute bars grouped per day per symbol.
	type dayKey = string
	byDay := map[dayKey]map[string][]Bar{}
	sessionOpen := map[dayKey]int64{}
	for sym, bars := range minuteBars {
		for _, b := range bars {
			n := time.Unix(b.Time, 0).In(loc)
			if n.Weekday() == time.Saturday || n.Weekday() == time.Sunday {
				continue
			}
			o := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, loc).Unix()
			c := o + regularSessionMin*60
			if b.Time < o || b.Time >= c {
				continue
			}
			d := dayOf(b.Time)
			if byDay[d] == nil {
				byDay[d] = map[string][]Bar{}
				sessionOpen[d] = o
			}
			byDay[d][sym] = append(byDay[d][sym], b)
		}
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		if (cfg.DayFrom != "" && d < cfg.DayFrom) || (cfg.DayTo != "" && d > cfg.DayTo) {
			continue
		}
		days = append(days, d)
	}
	sort.Strings(days)

	// Trailing daily context: for each symbol, dailyContext[day] = stats computed from
	// bars strictly BEFORE that day (no lookahead).
	dailyCtx := map[string]map[string]dailyStats{}
	for sym, dbars := range dailyBars {
		sort.Slice(dbars, func(i, j int) bool { return dbars[i].Time < dbars[j].Time })
		m := map[string]dailyStats{}
		for i := range dbars {
			d := dayOf(dbars[i].Time)
			m[d] = trailingStats(dbars[:i]) // bars before this day only
		}
		dailyCtx[sym] = m
	}
	// For days with minute bars but no daily bar (shouldn't happen), fall back to the
	// stats of the latest prior daily bar.
	statsFor := func(sym, day string) dailyStats {
		if s, ok := dailyCtx[sym][day]; ok {
			return s
		}
		best, out := "", dailyStats{}
		for d, s := range dailyCtx[sym] {
			if d < day && d > best {
				best, out = d, s
			}
		}
		return out
	}

	res := &BTResult{PerStrategy: map[string]*StratStats{}}
	for _, st := range strats {
		res.PerStrategy[st.Name()] = &StratStats{}
	}
	dayTracker := risk.NewDay(cfg.Limits, loc)

	// Optional ML dataset sink + walk-forward gate. Counterfactual tracking runs when
	// either needs it; resolved rows accumulate in-memory as the gate's training pool.
	var dataset *os.File
	if cfg.DatasetPath != "" {
		if f, err := os.Create(cfg.DatasetPath); err == nil {
			dataset = f
			defer f.Close()
		}
	}
	var gate *Gate
	if cfg.MLGate {
		gate = NewGate()
		if cfg.MLGateMargin != 0 {
			gate.Margin = cfg.MLGateMargin
		}
		res.GateOn = true
	}
	margin := cfg.MLGateMargin
	if margin == 0 {
		margin = 0.03
	}
	if len(cfg.Predictions) > 0 {
		res.GateOn = true
	}
	// The conditioning mechanisms learn from resolved counterfactual rows, so any of
	// them being on requires counterfactual tracking.
	trackCF := dataset != nil || gate != nil || len(cfg.Predictions) > 0 || cfg.EnsembleAgreement ||
		cfg.TODFilter || cfg.RegimeRouter || cfg.Throttle

	// #2: causal per-strategy score thresholds — for each day, the q-quantile of that
	// strategy's prediction scores on STRICTLY PRIOR days.
	var topqThreshold map[string]map[string]float64 // strategy -> day -> threshold
	if cfg.MLTopQuantile > 0 && len(cfg.Predictions) > 0 {
		topqThreshold = buildTopqThresholds(cfg.Predictions, cfg.MLTopQuantile, loc)
	}

	// P2.2: same causal-quantile mechanism, applied to the rank leg of the ensemble
	// agreement filter.
	ensembleClfMargin := cfg.EnsembleClfMargin
	if ensembleClfMargin == 0 {
		ensembleClfMargin = 0.03
	}
	ensembleRankQuantile := cfg.EnsembleRankQuantile
	if ensembleRankQuantile == 0 {
		ensembleRankQuantile = 0.70
	}
	var ensembleRankThreshold map[string]map[string]float64
	if cfg.EnsembleAgreement {
		res.GateOn = true
		ensembleRankThreshold = buildTopqThresholds(cfg.PredictionsRank, ensembleRankQuantile, loc)
	}

	// Online conditioning statistics (#3, #4, #6), fed by writeRow as outcomes resolve.
	todStats := map[string]*runStat{}      // "strategy|halfHourBucket"
	routerStats := map[string]*runStat{}   // "strategy|dayState"
	throttleStats := map[string]*ewmaStat{} // "strategy"
	var trainRows []DatasetRow
	writeRow := func(cf *cfTrack, exit float64, exitTime int64, reason string) {
		risk := cf.sig.Suggested.Entry - cf.sig.Suggested.Stop
		r := 0.0
		if risk > 0 {
			r = (exit - cf.sig.Suggested.Entry) / risk
		}
		row := DatasetRow{
			Day: cf.day, Strategy: cf.sig.Strategy, Symbol: cf.sig.Symbol, Sector: uni.Sector(cf.sig.Symbol),
			Time: cf.sig.Time, Entry: cf.sig.Suggested.Entry, Stop: cf.sig.Suggested.Stop,
			Target: cf.sig.Suggested.Target, Features: cf.sig.Features,
			Outcome: reason, ExitPrice: exit, RMultiple: r,
			MinutesHeld: (exitTime - cf.sig.Time) / 60, Traded: cf.traded,
		}
		trainRows = append(trainRows, row)
		// Feed the online conditioning stats (#3/#4/#6) — timeline-respecting: this
		// outcome is only known now, and only future entries see the updated stat.
		if k := cf.sig.Strategy + "|" + strconv.Itoa(cf.todBucket); true {
			if todStats[k] == nil {
				todStats[k] = &runStat{}
			}
			todStats[k].add(r)
		}
		if k := cf.sig.Strategy + "|" + strconv.Itoa(cf.dayState); true {
			if routerStats[k] == nil {
				routerStats[k] = &runStat{}
			}
			routerStats[k].add(r)
		}
		if throttleStats[cf.sig.Strategy] == nil {
			throttleStats[cf.sig.Strategy] = &ewmaStat{}
		}
		throttleStats[cf.sig.Strategy].add(r)
		if res.GateOn {
			ss := res.PerStrategy[cf.sig.Strategy]
			switch cf.gate {
			case 0:
				res.GateWarmup++
			case 1:
				res.GateAccepted++
				res.GateAcceptedAvgR += r // sums now; averaged after the loop
				if ss != nil {
					ss.GateAccepted++
					ss.GateAcceptedAvgR += r
				}
			case 2:
				res.GateRejected++
				res.GateRejectedAvgR += r
				if ss != nil {
					ss.GateRejected++
					ss.GateRejectedAvgR += r
				}
			}
		}
		if dataset != nil {
			if b, err := json.Marshal(row); err == nil {
				_, _ = dataset.Write(append(b, '\n'))
			}
		}
	}

	// Cooldowns mirror the live engine so backtest signal flow matches what shadow
	// logging will produce (30-min per strategy|symbol, max 2/day).
	cool := map[string]int64{}
	dayCnt := map[string]int{}

	// Regime posture per day: risk-on iff QQQ's PRIOR close is above its 20-day SMA of
	// prior closes. Uses daily bars only — no intraday lookahead. Needed by both the
	// hard filter and the soft router (#4).
	riskOn := map[string]bool{}
	if cfg.RegimeFilter || cfg.RegimeRouter {
		res.RegimeOn = cfg.RegimeFilter
		qqq := append([]Bar(nil), dailyBars["QQQ"]...)
		sort.Slice(qqq, func(i, j int) bool { return qqq[i].Time < qqq[j].Time })
		for _, day := range days {
			// Closes strictly before this day.
			var closes []float64
			for _, b := range qqq {
				if dayOf(b.Time) >= day {
					break
				}
				closes = append(closes, b.Close)
			}
			if len(closes) < 21 {
				riskOn[day] = true // not enough history — fail open
				continue
			}
			var sma float64
			for _, c := range closes[len(closes)-20:] {
				sma += c
			}
			sma /= 20
			riskOn[day] = closes[len(closes)-1] > sma
		}
	}

	for _, day := range days {
		// Walk-forward retrain: models see ONLY outcomes resolved on prior days.
		if gate != nil {
			gate.Train(trainRows)
		}
		open := sessionOpen[day]
		syms := byDay[day]
		standDown := cfg.RegimeFilter && !riskOn[day]
		if standDown {
			res.StandDownDays++
		}

		// Merge the day's bars into one time-ordered event stream.
		type ev struct {
			sym string
			bar Bar
		}
		var events []ev
		for sym, bars := range syms {
			sort.Slice(bars, func(i, j int) bool { return bars[i].Time < bars[j].Time })
			for _, b := range bars {
				events = append(events, ev{sym, b})
			}
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].bar.Time != events[j].bar.Time {
				return events[i].bar.Time < events[j].bar.Time
			}
			return events[i].sym < events[j].sym
		})

		session := map[string][]Bar{}  // per-symbol bars so far today
		cumVol := map[string]float64{} // per-symbol cumulative volume
		openPos := map[string]*btPosition{}
		cfOpen := map[string][]*cfTrack{}   // unresolved counterfactuals (dataset mode)
		pendings := map[string]*pendingEntry{} // resting passive limits (#5)
		dayStart := len(res.Trades)
		dayHalted := false
		dayState := 0 // #4: 1 = risk-on, 0 = risk-off
		if riskOn[day] {
			dayState = 1
		}

		closePos := func(p *btPosition, exit float64, exitTime int64, reason string, slipped bool) {
			if slipped {
				exit *= 1 - cfg.SlippageBps/10000
			}
			pnl := (exit - p.entry) * p.qty
			risk := p.entry - p.sig.Suggested.Stop
			r := 0.0
			if risk > 0 {
				r = (exit - p.entry) / risk
			}
			res.Trades = append(res.Trades, BTTrade{
				Strategy: p.sig.Strategy, Symbol: p.sig.Symbol, Day: day,
				EntryTime: p.sig.Time, ExitTime: exitTime, Entry: p.entry, Exit: exit,
				Stop: p.sig.Suggested.Stop, Target: p.sig.Suggested.Target,
				Qty: p.qty, PNL: pnl, R: r, ExitReason: reason,
			})
			delete(openPos, p.sig.Symbol)
			dayTracker.OnRealized(pnl, time.Unix(exitTime, 0))
			if _, halted := dayTracker.Realized(time.Unix(exitTime, 0)); halted {
				dayHalted = true
			}
		}

		for _, evt := range events {
			sym, b := evt.sym, evt.bar
			session[sym] = append(session[sym], b)
			cumVol[sym] += b.Volume
			bars := session[sym]

			// 0) Resolve counterfactual brackets for this symbol (dataset mode).
			if list := cfOpen[sym]; len(list) > 0 {
				remaining := list[:0]
				for _, cf := range list {
					if b.Time <= cf.sig.Time {
						remaining = append(remaining, cf)
						continue
					}
					exit, reason := 0.0, ""
					switch {
					case b.Low <= cf.sig.Suggested.Stop:
						exit, reason = cf.sig.Suggested.Stop, "stop"
					case b.High >= cf.sig.Suggested.Target:
						exit, reason = cf.sig.Suggested.Target, "target"
					case cf.sig.MaxHoldMin > 0 && (b.Time-cf.sig.Time)/60 >= int64(cf.sig.MaxHoldMin):
						exit, reason = b.Close, "time"
					case minuteOf(b.Time, open) >= eodFlattenMin:
						exit, reason = b.Close, "eod"
					}
					if reason == "" {
						remaining = append(remaining, cf)
						continue
					}
					writeRow(cf, exit, b.Time, reason)
				}
				cfOpen[sym] = remaining
			}

			// 1) Manage an open position in this symbol (first-touch; stop wins ties).
			if p, ok := openPos[sym]; ok && b.Time > p.sig.Time {
				switch {
				case b.Low <= p.sig.Suggested.Stop:
					closePos(p, p.sig.Suggested.Stop, b.Time, "stop", true)
				case b.High >= p.sig.Suggested.Target:
					closePos(p, p.sig.Suggested.Target, b.Time, "target", false)
				case p.sig.MaxHoldMin > 0 && (b.Time-p.sig.Time)/60 >= int64(p.sig.MaxHoldMin):
					closePos(p, b.Close, b.Time, "time", true)
				case minuteOf(b.Time, open) >= eodFlattenMin:
					closePos(p, b.Close, b.Time, "eod", true)
				}
			}

			// 1b) Passive limit orders (#5): fill if this bar traded through the limit,
			// expire after the resting window. Slots and the loss cap are re-checked at
			// FILL time (the order doesn't reserve a slot while resting). On the fill
			// bar only the STOP is checked (intrabar entry→target order is unknowable;
			// counting a same-bar target would be optimistic, a same-bar stop is not).
			if pe := pendings[sym]; pe != nil && b.Time > pe.sig.Time {
				switch {
				case b.Time > pe.expires:
					delete(pendings, sym)
					res.PassiveMisses++
				case b.Low <= pe.sig.Suggested.Entry:
					delete(pendings, sym)
					_, held := openPos[sym]
					if !held && dayTracker.CanEnter(len(openPos), time.Unix(b.Time, 0)) == nil {
						qty := cfg.Limits.Size(pe.sig.Suggested.Entry, pe.sig.Suggested.Stop)
						if cfg.Throttle {
							if ts := throttleStats[pe.sig.Strategy]; ts != nil && ts.halves() {
								qty = float64(int(qty / 2))
								res.ThrottledHalf++
							}
						}
						if qty >= 1 {
							p := &btPosition{sig: pe.sig, entry: pe.sig.Suggested.Entry, qty: qty}
							openPos[sym] = p
							res.PassiveFills++
							if pe.cf != nil {
								pe.cf.traded = true
							}
							if b.Low <= pe.sig.Suggested.Stop {
								closePos(p, pe.sig.Suggested.Stop, b.Time, "stop", true)
							}
						} else {
							res.SkippedSize++
						}
					} else {
						res.SkippedRisk++
					}
				}
			}

			// 2) Detection — context symbols only feed the market backdrop.
			if !uni.Has(sym) {
				continue
			}
			st := statsFor(sym, day)
			if st.ATR <= 0 || !tradable(b.Close, st.AvgVol) {
				continue
			}
			frac := clampF((float64(b.Time-open)/60+1)/regularSessionMin, 1.0/regularSessionMin, 1)
			mktOK, mktPct := marketState(session["QQQ"])
			ctx := Context{
				SessionOpen: open,
				ATR:         st.ATR,
				AvgVolume:   st.AvgVol,
				RVOL:        cumVol[sym] / (st.AvgVol * frac),
				MarketOK:    mktOK,
				MarketPct:   mktPct,
			}
			for _, strat := range strats {
				sig := strat.Detect(sym, bars, ctx)
				if sig == nil {
					continue
				}
				if cfg.SectorLeadLag {
					if extra := sectorLeadLagFeatures(uni, sym, func(s string) []Bar { return session[s] }); extra != nil {
						for k, v := range extra {
							if _, exists := sig.Features[k]; !exists {
								sig.Features[k] = v
							}
						}
					}
				}
				// Same cooldowns as the live engine, so signal flow matches shadow logs.
				key := sig.Strategy + "|" + sym
				if last, ok := cool[key]; ok && sig.Time-last < int64(signalCooldown.Seconds()) {
					continue
				}
				if dayCnt[key+"|"+day] >= maxPerDay {
					continue
				}
				cool[key] = sig.Time
				dayCnt[key+"|"+day]++
				res.PerStrategy[strat.Name()].Signals++
				todBucket := minuteOf(sig.Time, open) / 30
				var cf *cfTrack
				if trackCF {
					cf = &cfTrack{sig: *sig, day: day, todBucket: todBucket, dayState: dayState}
					cfOpen[sym] = append(cfOpen[sym], cf)
				}
				// ML gate: trade only when the model expects positive R (absolute margin)
				// or the score clears the causal per-strategy top quantile (#2). Signals
				// still in warmup (no model / no score / no threshold yet) trade ungated.
				var gateOK, gateApplied bool
				var predR float64
				if gate != nil {
					gateOK, predR, gateApplied = gate.Allow(*sig)
				} else if len(cfg.Predictions) > 0 {
					if v, found := cfg.Predictions[sig.Strategy+"|"+sym+"|"+strconv.FormatInt(sig.Time, 10)]; found {
						predR = v
						if topqThreshold != nil {
							if thr, ok := topqThreshold[sig.Strategy][day]; ok {
								gateApplied = true
								gateOK = v >= thr
							}
						} else {
							gateApplied = true
							gateOK = v >= margin
						}
					}
				} else if cfg.EnsembleAgreement {
					// P2.2: all three legs must agree. Any leg missing a score, or the rank
					// leg's causal threshold not yet established for this (strategy, day),
					// is warmup pass-through — same semantics as the single-model gates.
					predKey := sig.Strategy + "|" + sym + "|" + strconv.FormatInt(sig.Time, 10)
					clfV, clfOK := cfg.PredictionsClf[predKey]
					rankV, rankOK := cfg.PredictionsRank[predKey]
					regV, regOK := cfg.PredictionsReg[predKey]
					if clfOK && rankOK && regOK {
						if thr, ok := ensembleRankThreshold[sig.Strategy][day]; ok {
							gateApplied = true
							predR = clfV
							gateOK = clfV >= ensembleClfMargin && rankV >= thr && regV >= 0
						}
					}
				}
				if gateApplied && cf != nil {
					cf.predR = predR
					if gateOK {
						cf.gate = 1
					} else {
						cf.gate = 2
					}
				}
				if gateApplied && !gateOK {
					continue
				}
				// Operator rule: no entries before session minute N.
				if cfg.MinEntryMinute > 0 && minuteOf(sig.Time, open) < cfg.MinEntryMinute {
					res.SkippedEarly++
					continue
				}
				// #3: block (strategy, time-of-day) buckets with proven negative expectancy.
				if cfg.TODFilter {
					if st := todStats[sig.Strategy+"|"+strconv.Itoa(todBucket)]; st != nil && st.blocks() {
						res.SkippedTOD++
						continue
					}
				}
				// #4: block (strategy, market-state) pairs with proven negative expectancy.
				if cfg.RegimeRouter {
					if st := routerStats[sig.Strategy+"|"+strconv.Itoa(dayState)]; st != nil && st.blocks() {
						res.SkippedRouter++
						continue
					}
				}
				if standDown {
					res.SkippedRegime++
					continue // posture brake: signal logged, no entry
				}
				if _, held := openPos[sym]; held {
					continue // one position per symbol
				}
				if pendings[sym] != nil {
					continue // one working order per symbol
				}
				// #5: passive entry — rest a limit at the signal price instead of paying
				// the spread; slots/loss-cap are re-checked at fill time.
				if cfg.PassiveEntry {
					pendings[sym] = &pendingEntry{sig: *sig, cf: cf, expires: sig.Time + passiveWindowMin*60}
					res.PassiveAttempts++
					continue
				}
				if err := dayTracker.CanEnter(len(openPos), time.Unix(b.Time, 0)); err != nil {
					res.SkippedRisk++
					continue
				}
				qty := cfg.Limits.Size(sig.Suggested.Entry, sig.Suggested.Stop)
				// #6: half-size strategies whose realized-R EWMA has gone negative.
				if cfg.Throttle {
					if ts := throttleStats[sig.Strategy]; ts != nil && ts.halves() {
						qty = float64(int(qty / 2))
						res.ThrottledHalf++
					}
				}
				if qty < 1 {
					res.SkippedSize++
					continue
				}
				entry := sig.Suggested.Entry * (1 + cfg.SlippageBps/10000)
				openPos[sym] = &btPosition{sig: *sig, entry: entry, qty: qty}
				if cf != nil {
					cf.traded = true
				}
			}
		}

		// Passive limits still resting at the close expire unfilled.
		res.PassiveMisses += len(pendings)

		// Safety: close anything still open at its last seen price (thin data days).
		for _, p := range openPos {
			bars := session[p.sig.Symbol]
			last := bars[len(bars)-1]
			closePos(p, last.Close, last.Time, "eod", true)
		}
		for sym, list := range cfOpen {
			bars := session[sym]
			if len(bars) == 0 {
				continue
			}
			last := bars[len(bars)-1]
			for _, cf := range list {
				writeRow(cf, last.Close, last.Time, "eod")
			}
		}

		var dayPNL float64
		for _, t := range res.Trades[dayStart:] {
			dayPNL += t.PNL
		}
		res.Days = append(res.Days, DayPNL{Day: day, Trades: len(res.Trades) - dayStart, PNL: dayPNL, Halted: dayHalted})
	}

	// Aggregate.
	for _, t := range res.Trades {
		s := res.PerStrategy[t.Strategy]
		if s == nil {
			s = &StratStats{}
			res.PerStrategy[t.Strategy] = s
		}
		s.Trades++
		s.TotalPNL += t.PNL
		s.AvgR += t.R
		s.AvgMinutes += float64(t.ExitTime-t.EntryTime) / 60
		switch {
		case t.PNL > 0:
			s.Wins++
			s.AvgWin += t.PNL
		case t.PNL < 0:
			s.Losses++
			s.AvgLoss += t.PNL
		}
		switch t.ExitReason {
		case "eod":
			s.Timeouts++
		case "time":
			s.TimeExits++
		}
		res.TotalPNL += t.PNL
	}
	for _, s := range res.PerStrategy {
		if s.Trades > 0 {
			s.HitRate = float64(s.Wins) / float64(s.Trades) * 100
			s.AvgPNL = s.TotalPNL / float64(s.Trades)
			s.AvgR /= float64(s.Trades)
			s.AvgMinutes /= float64(s.Trades)
		}
		if s.Wins > 0 {
			s.AvgWin /= float64(s.Wins)
		}
		if s.Losses > 0 {
			s.AvgLoss /= float64(s.Losses)
		}
	}
	if n := len(res.Days); n > 0 {
		res.AvgDayPNL = res.TotalPNL / float64(n)
	}
	// Max drawdown on the daily equity curve.
	equity, peak, maxDD := 0.0, 0.0, 0.0
	for _, d := range res.Days {
		equity += d.PNL
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}
	res.MaxDrawdown = maxDD
	if res.GateAccepted > 0 {
		res.GateAcceptedAvgR /= float64(res.GateAccepted)
	}
	if res.GateRejected > 0 {
		res.GateRejectedAvgR /= float64(res.GateRejected)
	}
	for _, s := range res.PerStrategy {
		if s.GateAccepted > 0 {
			s.GateAcceptedAvgR /= float64(s.GateAccepted)
		}
		if s.GateRejected > 0 {
			s.GateRejectedAvgR /= float64(s.GateRejected)
		}
	}
	return res
}

// buildTopqThresholds computes, per strategy per day, the q-quantile of that strategy's
// prediction scores on strictly prior days (the causal top-K approximation for #2).
// Days with fewer than condMinScores prior scores get no threshold (warmup pass-through).
func buildTopqThresholds(preds map[string]float64, q float64, loc *time.Location) map[string]map[string]float64 {
	type scored struct {
		day   string
		score float64
	}
	byStrat := map[string][]scored{}
	for k, v := range preds {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			continue
		}
		t, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue
		}
		day := time.Unix(t, 0).In(loc).Format("2006-01-02")
		byStrat[parts[0]] = append(byStrat[parts[0]], scored{day, v})
	}
	out := map[string]map[string]float64{}
	for strat, list := range byStrat {
		sort.Slice(list, func(i, j int) bool { return list[i].day < list[j].day })
		out[strat] = map[string]float64{}
		var prior []float64
		i := 0
		for i < len(list) {
			day := list[i].day
			if len(prior) >= condMinScores {
				sorted := append([]float64(nil), prior...)
				sort.Float64s(sorted)
				idx := int(q * float64(len(sorted)-1))
				out[strat][day] = sorted[idx]
			}
			for i < len(list) && list[i].day == day {
				prior = append(prior, list[i].score)
				i++
			}
		}
	}
	return out
}

// trailingStats computes ATR(14) + 20-day avg volume from prior daily bars.
func trailingStats(prior []Bar) dailyStats {
	n := len(prior)
	if n < 5 {
		return dailyStats{}
	}
	var trs []float64
	lo := n - 15
	if lo < 1 {
		lo = 1
	}
	for i := lo; i < n; i++ {
		tr := prior[i].High - prior[i].Low
		if x := math.Abs(prior[i].High - prior[i-1].Close); x > tr {
			tr = x
		}
		if x := math.Abs(prior[i-1].Close - prior[i].Low); x > tr {
			tr = x
		}
		trs = append(trs, tr)
	}
	var atr float64
	for _, v := range trs {
		atr += v
	}
	if len(trs) > 0 {
		atr /= float64(len(trs))
	}
	var vol float64
	vlo := n - 20
	if vlo < 0 {
		vlo = 0
	}
	for i := vlo; i < n; i++ {
		vol += prior[i].Volume
	}
	vol /= float64(n - vlo)
	return dailyStats{ATR: atr, AvgVol: vol}
}

// marketState mirrors Store.Market for the backtester's per-day QQQ session.
func marketState(qqq []Bar) (bool, float64) {
	if len(qqq) < 5 {
		return true, 0
	}
	vw := vwapSeries(qqq)
	last := qqq[len(qqq)-1]
	pct := 0.0
	if open := qqq[0].Open; open > 0 {
		pct = (last.Close - open) / open * 100
	}
	return last.Close >= vw[len(vw)-1] || pct >= 0, pct
}

// Report renders a human-readable summary.
func (r *BTResult) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n════════ BACKTEST — %d days, %d trades ════════\n", len(r.Days), len(r.Trades))
	fmt.Fprintf(&b, "Total P&L: $%.2f   ·   Avg/day: $%.2f   ·   Max drawdown: $%.2f\n", r.TotalPNL, r.AvgDayPNL, r.MaxDrawdown)
	fmt.Fprintf(&b, "Skipped by risk caps: %d   ·   Unfundable (<1 share): %d\n", r.SkippedRisk, r.SkippedSize)
	if r.RegimeOn {
		fmt.Fprintf(&b, "Regime filter: %d stand-down days · %d signals blocked\n", r.StandDownDays, r.SkippedRegime)
	}
	if r.SkippedEarly > 0 {
		fmt.Fprintf(&b, "Min-entry-minute rule: %d entries blocked\n", r.SkippedEarly)
	}
	if r.SkippedTOD > 0 {
		fmt.Fprintf(&b, "Time-of-day filter: %d entries blocked\n", r.SkippedTOD)
	}
	if r.SkippedRouter > 0 {
		fmt.Fprintf(&b, "Regime router: %d entries blocked\n", r.SkippedRouter)
	}
	if r.PassiveAttempts > 0 {
		fmt.Fprintf(&b, "Passive entry: %d rested · %d filled (%.0f%%) · %d missed\n",
			r.PassiveAttempts, r.PassiveFills, float64(r.PassiveFills)/float64(r.PassiveAttempts)*100, r.PassiveMisses)
	}
	if r.ThrottledHalf > 0 {
		fmt.Fprintf(&b, "Sizing throttle: %d entries half-sized\n", r.ThrottledHalf)
	}
	if r.GateOn {
		fmt.Fprintf(&b, "ML gate: accepted %d (cf avg R %+.3f)  ·  rejected %d (cf avg R %+.3f)  ·  warmup %d\n",
			r.GateAccepted, r.GateAcceptedAvgR, r.GateRejected, r.GateRejectedAvgR, r.GateWarmup)
		names := make([]string, 0, len(r.PerStrategy))
		for n := range r.PerStrategy {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := r.PerStrategy[n]
			if s.GateAccepted+s.GateRejected == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %-16s accepted %4d (R %+.3f) · rejected %4d (R %+.3f)\n",
				n, s.GateAccepted, s.GateAcceptedAvgR, s.GateRejected, s.GateRejectedAvgR)
		}
	}
	b.WriteString("\n")

	names := make([]string, 0, len(r.PerStrategy))
	for n := range r.PerStrategy {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "%-16s %8s %7s %8s %9s %9s %9s %7s %5s %5s\n", "strategy", "signals", "trades", "hit%", "totalP&L", "avgP&L", "avgR", "avgMin", "eod", "time")
	for _, n := range names {
		s := r.PerStrategy[n]
		fmt.Fprintf(&b, "%-16s %8d %7d %7.1f%% %9.2f %9.2f %9.2f %7.0f %5d %5d\n",
			n, s.Signals, s.Trades, s.HitRate, s.TotalPNL, s.AvgPNL, s.AvgR, s.AvgMinutes, s.Timeouts, s.TimeExits)
	}
	b.WriteString("\nPer-day P&L:\n")
	for _, d := range r.Days {
		mark := ""
		if d.Halted {
			mark = "  ← loss cap hit"
		}
		fmt.Fprintf(&b, "  %s  %3d trades  $%9.2f%s\n", d.Day, d.Trades, d.PNL, mark)
	}
	return b.String()
}
