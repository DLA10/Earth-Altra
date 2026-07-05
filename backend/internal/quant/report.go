package quant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReasonStat aggregates closed trades by their exit reason.
type ReasonStat struct {
	Count    int     `json:"count"`
	TotalPNL float64 `json:"total_pnl"`
	AvgPNL   float64 `json:"avg_pnl"`
}

// ExitAttribution answers "is Agent 3 adding value?" by comparing the average realized P&L of the
// agent's DISCRETIONARY exits (AI_Exit / Take_Profit) against the pure protective STOP exits
// (Trail_Stop / Stop). If the discretionary average beats the stop average, the LLM exits earn
// their keep; if not, the dumb stop would have done better.
type ExitAttribution struct {
	ByReason           map[string]*ReasonStat `json:"by_reason"`
	DiscretionaryAvg   float64                `json:"discretionary_avg_pnl"`
	DiscretionaryCount int                    `json:"discretionary_count"`
	StopAvg            float64                `json:"stop_avg_pnl"`
	StopCount          int                    `json:"stop_count"`
	Agent3AddsValue    bool                   `json:"agent3_adds_value"`
}

// SourceStat aggregates realized outcomes for one entry pipeline (dip or signal).
type SourceStat struct {
	Trades   int     `json:"trades"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"win_rate"`
	TotalPNL float64 `json:"total_pnl"`
	AvgPNL   float64 `json:"avg_pnl"`
}

// DipScorecard answers "is the dip ENTRY agent (Agent 2) making useful decisions, or is it
// mostly catching falling knives?" It reads the source-tagged close outcomes the manager
// journals, plus Agent 2's own approve/reject decisions, over a rolling window. KnifeRate
// = share of taken dips that ended in a loss (the dip kept falling into the stop); a good
// dip-picker keeps this low. Signal-pipeline stats are shown alongside for comparison.
type DipScorecard struct {
	WindowDays int        `json:"window_days"`
	Decisions  int        `json:"decisions"`      // Agent 2 entry decisions seen
	Approved   int        `json:"approved"`       // ...that said buy
	Rejected   int        `json:"rejected"`       // ...that said no_buy
	AvgConf    float64    `json:"avg_confidence"` // mean conviction on approvals
	Dip        SourceStat `json:"dip"`            // realized outcomes of dip-pipeline trades
	Signal     SourceStat `json:"signal"`         // realized outcomes of signal-pipeline trades
	Rehydrated SourceStat `json:"rehydrated"`     // positions adopted after a restart (unknown origin)
	KnifeRate  float64    `json:"knife_rate"`     // losing dip trades / dip trades
	Verdict    string     `json:"verdict"`        // plain-language read for the page
}

// AgentInfo is one row in the "who's on the desk" panel: which model runs it and what it does.
type AgentInfo struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Role  string `json:"role"`
	Live  bool   `json:"live"` // enabled/active right now
}

// QuantReport is the full state the Paper·Claude page renders.
type QuantReport struct {
	Live         bool            `json:"live"`
	UniverseSize int             `json:"universe_size"`
	Posture      string          `json:"posture"`
	Alloc        AllocSnapshot   `json:"alloc"`
	State        QuantState      `json:"state"`
	Attribution  ExitAttribution `json:"attribution"`
	DipScore     *DipScorecard   `json:"dip_score,omitempty"`
	Agents       []AgentInfo     `json:"agents,omitempty"`
	Review       *Review         `json:"review,omitempty"`
}

// Report assembles the current quant pipeline state (realized-only P&L + attribution + latest
// review). Read-only.
func (e *Engine) Report() QuantReport {
	r := QuantReport{Live: e.live}
	if e.universe != nil {
		r.UniverseSize = len(e.universe.Symbols())
		r.Posture = e.universe.Regime().Posture
	}
	if e.alloc != nil {
		r.Alloc = e.alloc.Snapshot()
	}
	if e.broker != nil && e.broker.Enabled() {
		if st, err := e.broker.Reconstruct(e.LastClose); err == nil {
			r.State = st
		}
	}
	r.Attribution = attribution(r.State.Trades)
	r.DipScore = e.dipScorecard(20)
	r.Review = e.latestReview()
	return r
}

// dipScorecard scans the last windowDays of decision logs for Agent 2's entry decisions
// and the manager's source-tagged close outcomes, and reports how the dip pipeline is
// actually doing (with the signal pipeline alongside for comparison).
func (e *Engine) dipScorecard(windowDays int) *DipScorecard {
	dir := e.dataDir
	if dir == "" {
		dir = "data"
	}
	entries, err := os.ReadDir(filepath.Join(dir, "decisions"))
	if err != nil {
		return nil
	}
	// Newest windowDays files (filenames are YYYY-MM-DD.jsonl → lexical = chronological).
	var names []string
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".jsonl") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	if len(names) > windowDays {
		names = names[len(names)-windowDays:]
	}

	sc := &DipScorecard{WindowDays: windowDays}
	var confSum float64
	src := map[string]*SourceStat{"dip": &sc.Dip, "signal": &sc.Signal, "rehydrated": &sc.Rehydrated}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, "decisions", n))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec struct {
				Agent  string          `json:"agent"`
				Event  string          `json:"event"`
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			switch {
			case rec.Agent == "agent2_entry" && rec.Event == "decision":
				var d struct {
					Action     string  `json:"action"`
					Confidence float64 `json:"confidence"`
				}
				_ = json.Unmarshal(rec.Output, &d)
				sc.Decisions++
				if strings.EqualFold(d.Action, "buy") {
					sc.Approved++
					confSum += d.Confidence
				} else {
					sc.Rejected++
				}
			case rec.Agent == "pipeline" && rec.Event == "outcome":
				var o struct {
					Source string  `json:"source"`
					PNL    float64 `json:"pnl"`
					Win    bool    `json:"win"`
				}
				if json.Unmarshal(rec.Output, &o) != nil {
					continue
				}
				st := src[o.Source]
				if st == nil {
					continue // untagged/rehydrated legacy record
				}
				st.Trades++
				st.TotalPNL = round2(st.TotalPNL + o.PNL)
				if o.Win {
					st.Wins++
				}
			}
		}
	}
	if sc.Approved > 0 {
		sc.AvgConf = round2(confSum / float64(sc.Approved))
	}
	for _, st := range src {
		if st.Trades > 0 {
			st.WinRate = round2(float64(st.Wins) / float64(st.Trades))
			st.AvgPNL = round2(st.TotalPNL / float64(st.Trades))
		}
	}
	if sc.Dip.Trades > 0 {
		sc.KnifeRate = round2(float64(sc.Dip.Trades-sc.Dip.Wins) / float64(sc.Dip.Trades))
	}
	sc.Verdict = dipVerdict(sc)
	return sc
}

// dipVerdict is a plain-language read a novice can act on.
func dipVerdict(sc *DipScorecard) string {
	if sc.Dip.Trades < 5 {
		return "Not enough dip trades yet to judge — collecting data."
	}
	switch {
	case sc.Dip.TotalPNL > 0 && sc.KnifeRate <= 0.5:
		return "Healthy: the dip agent is net positive and mostly catching real bounces, not knives."
	case sc.Dip.TotalPNL > 0:
		return "Net positive, but its win rate is low — profits are riding a few winners; watch it."
	case sc.KnifeRate > 0.6:
		return "Concern: most dip trades are losing (catching falling knives). Consider leaning on the measured dip_bounce strategy instead."
	default:
		return "Net negative so far — the dip agent is not adding value in this regime."
	}
}

func attribution(trades []QuantTrade) ExitAttribution {
	att := ExitAttribution{ByReason: map[string]*ReasonStat{}}
	var dSum, sSum float64
	for _, t := range trades {
		st := att.ByReason[t.ExitReason]
		if st == nil {
			st = &ReasonStat{}
			att.ByReason[t.ExitReason] = st
		}
		st.Count++
		st.TotalPNL = round2(st.TotalPNL + t.PNL)
		switch t.ExitReason {
		case "AI_Exit", "Take_Profit":
			att.DiscretionaryCount++
			dSum += t.PNL
		case "Trail_Stop", "Stop":
			att.StopCount++
			sSum += t.PNL
		}
	}
	for _, st := range att.ByReason {
		if st.Count > 0 {
			st.AvgPNL = round2(st.TotalPNL / float64(st.Count))
		}
	}
	if att.DiscretionaryCount > 0 {
		att.DiscretionaryAvg = round2(dSum / float64(att.DiscretionaryCount))
	}
	if att.StopCount > 0 {
		att.StopAvg = round2(sSum / float64(att.StopCount))
	}
	// Only a meaningful verdict when BOTH kinds of exit exist to compare.
	att.Agent3AddsValue = att.DiscretionaryCount > 0 && att.StopCount > 0 && att.DiscretionaryAvg >= att.StopAvg
	return att
}

func (e *Engine) latestReview() *Review {
	dir := e.dataDir
	if dir == "" {
		dir = "data"
	}
	entries, err := os.ReadDir(filepath.Join(dir, "reviews"))
	if err != nil {
		return nil
	}
	latest := ""
	for _, en := range entries {
		if n := en.Name(); strings.HasSuffix(n, ".json") && n > latest {
			latest = n
		}
	}
	if latest == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "reviews", latest))
	if err != nil {
		return nil
	}
	var rv Review
	if json.Unmarshal(b, &rv) != nil {
		return nil
	}
	return &rv
}
