package quant

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	Rise       SourceStat `json:"rise"`           // realized outcomes of rise-watcher trades
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

// DeskReport is one desk's (paper account's) headline line: the signal desk and the
// dip+rise desk each trade their own account with their own allocator and loss cap.
type DeskReport struct {
	Name          string        `json:"name"` // "signal" | "dip+rise"
	Live          bool          `json:"live"`
	Alloc         AllocSnapshot `json:"alloc"`
	Realized      float64       `json:"realized_pnl"`
	RealizedToday float64       `json:"realized_today"` // closed TODAY (ET) only
	Unrealized    float64       `json:"unrealized_pnl"`
	DayPnL        float64       `json:"account_day_pnl"` // Alpaca: equity − last_equity (broker truth)
	Trades        int           `json:"trades"`
}

// QuantReport is the full state the Paper·Claude page renders. Since the desk split,
// the page evaluates the SIGNAL desk: State/Alloc/Attribution cover the signal account
// only (the Dip+Rise page owns that desk's numbers). Desks still lists both accounts'
// headline P&L side by side so the team view is one glance away.
type QuantReport struct {
	Live         bool            `json:"live"`
	UniverseSize int             `json:"universe_size"`
	Posture      string          `json:"posture"`
	Alloc        AllocSnapshot   `json:"alloc"`
	State        QuantState      `json:"state"`
	Desks        []DeskReport    `json:"desks,omitempty"`
	Attribution  ExitAttribution `json:"attribution"`
	DipScore     *DipScorecard   `json:"dip_score,omitempty"`
	Agents       []AgentInfo     `json:"agents,omitempty"`
	Review       *Review         `json:"review,omitempty"`
}

// deskState reconstructs one broker's account (empty state when disabled/unreachable).
func (e *Engine) deskState(b *Broker) QuantState {
	if b == nil || !b.Enabled() {
		return QuantState{}
	}
	st, err := b.Reconstruct(e.LastClose)
	if err != nil {
		return QuantState{}
	}
	// Day slice of the cumulative number: what THIS session actually realized (ET day).
	day := time.Now().In(e.loc).Format("2006-01-02")
	for _, t := range st.Trades {
		if t.ExitTime.In(e.loc).Format("2006-01-02") == day {
			st.RealizedToday += t.PNL
		}
	}
	return st
}

// MergedState reconstructs the team's trades across BOTH desk accounts (dip+rise and
// signal) — the whole-team numbers the report and the daily reviewer read.
func (e *Engine) MergedState() QuantState {
	dip := e.deskState(e.broker)
	if e.sigBroker == nil {
		return dip
	}
	sig := e.deskState(e.sigBroker)
	m := QuantState{
		RealizedPNL:   dip.RealizedPNL + sig.RealizedPNL,
		RealizedToday: dip.RealizedToday + sig.RealizedToday,
		UnrealizedPNL: dip.UnrealizedPNL + sig.UnrealizedPNL,
		Positions:     append(append([]QuantPosition{}, dip.Positions...), sig.Positions...),
		Trades:        append(append([]QuantTrade{}, dip.Trades...), sig.Trades...),
	}
	sort.Slice(m.Positions, func(i, j int) bool { return m.Positions[i].Symbol < m.Positions[j].Symbol })
	sort.Slice(m.Trades, func(i, j int) bool { return m.Trades[i].ExitTime.Before(m.Trades[j].ExitTime) })
	wins := 0
	for _, t := range m.Trades {
		if t.PNL > 0 {
			wins++
		}
	}
	m.TotalTrades = len(m.Trades)
	if m.TotalTrades > 0 {
		m.WinRate = float64(wins) / float64(m.TotalTrades) * 100
	}
	return m
}

// Report assembles the current quant team state (realized-only P&L + attribution + latest
// review), whole-team merged with a per-desk breakdown. Read-only.
func (e *Engine) Report() QuantReport {
	r := QuantReport{Live: e.live || e.sigLive}
	if e.universe != nil {
		r.UniverseSize = len(e.universe.Symbols())
		r.Posture = e.universe.Regime().Posture
	}

	dip := e.deskState(e.broker)
	if e.sigBroker != nil || e.sigAlloc != nil {
		sig := e.deskState(e.sigBroker)
		var sigSnap AllocSnapshot
		if e.sigAlloc != nil {
			sigSnap = e.sigAlloc.Snapshot()
		}
		var dipSnap AllocSnapshot
		if e.alloc != nil {
			dipSnap = e.alloc.Snapshot()
		}
		r.Live = e.sigLive // the page is the signal desk's page
		r.Alloc = sigSnap
		// Day P&L per desk straight from Alpaca (equity vs prior close) — broker-level
		// truth that covers every share on the account, not just what we journaled.
		deskDay := func(b *Broker) float64 {
			if b == nil || !b.Enabled() {
				return 0
			}
			if ai, err := b.Account(); err == nil {
				return round2(ai.DayPnL())
			}
			return 0
		}
		r.Desks = []DeskReport{
			{Name: "signal", Live: e.sigLive, Alloc: sigSnap, DayPnL: deskDay(e.sigBroker),
				Realized: round2(sig.RealizedPNL), RealizedToday: round2(sig.RealizedToday),
				Unrealized: round2(sig.UnrealizedPNL), Trades: sig.TotalTrades},
			{Name: "dip+rise", Live: e.live, Alloc: dipSnap, DayPnL: deskDay(e.broker),
				Realized: round2(dip.RealizedPNL), RealizedToday: round2(dip.RealizedToday),
				Unrealized: round2(dip.UnrealizedPNL), Trades: dip.TotalTrades},
		}
		r.State = sig // SIGNAL account only — each desk's P&L stays its own
	} else {
		if e.alloc != nil {
			r.Alloc = e.alloc.Snapshot()
		}
		r.State = dip
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
	src := map[string]*SourceStat{"dip": &sc.Dip, "signal": &sc.Signal, "rise": &sc.Rise, "rehydrated": &sc.Rehydrated}
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

// DipRiseEvent is one recent decision-log line on the dip+rise desk's timeline: a
// detected dip, Agent 2's verdict, a rise arm/trigger/expiry, a funding decision, or a
// close outcome.
type DipRiseEvent struct {
	Time   string `json:"time"`
	Agent  string `json:"agent"`
	Event  string `json:"event"`
	Symbol string `json:"symbol"`
	Note   string `json:"note"`
}

// DipRiseReport is everything the Dip+Rise page renders: desk state (its own account),
// the dips currently armed for a rise confirmation, the dip scorecard, and the recent
// decision timeline.
type DipRiseReport struct {
	Enabled  bool           `json:"enabled"`   // PAPER_DIP broker keys present
	Live     bool           `json:"live"`      // dip pipeline placing paper orders
	RiseLive bool           `json:"rise_live"` // rise watcher placing paper orders
	Alloc    AllocSnapshot  `json:"alloc"`
	State    QuantState     `json:"state"` // the dip+rise account only
	DipScore *DipScorecard  `json:"dip_score,omitempty"`
	Armed    []RiseArmView  `json:"armed"`
	Events   []DipRiseEvent `json:"events"`
}

// dipRiseAgents are the decision-log agents that belong to the dip+rise desk's story.
// (allocator records come only from the dip funding path; the signal desk logs its
// funding under signal_trader.)
var dipRiseAgents = map[string]bool{
	"pipeline": true, "agent2_entry": true, "rise_watch": true, "allocator": true,
}

// DipRiseReport assembles the Dip+Rise page state. Read-only.
func (e *Engine) DipRiseReport() DipRiseReport {
	r := DipRiseReport{
		Enabled:  e.broker != nil && e.broker.Enabled(),
		Live:     e.live,
		RiseLive: e.rise.Live(),
		State:    e.deskState(e.broker),
		DipScore: e.dipScorecard(20),
		Armed:    e.rise.Armed(),
	}
	if r.Armed == nil {
		r.Armed = []RiseArmView{}
	}
	if e.alloc != nil {
		r.Alloc = e.alloc.Snapshot()
	}
	r.Events = e.dipRiseEvents(2, 200)
	return r
}

// dipRiseEvents reads the newest `days` decision logs and returns the dip+rise desk's
// lines, newest first, capped at max.
func (e *Engine) dipRiseEvents(days, max int) []DipRiseEvent {
	dir := e.dataDir
	if dir == "" {
		dir = "data"
	}
	entries, err := os.ReadDir(filepath.Join(dir, "decisions"))
	if err != nil {
		return []DipRiseEvent{}
	}
	var names []string
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".jsonl") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	if len(names) > days {
		names = names[len(names)-days:]
	}
	out := []DipRiseEvent{}
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
				Time   string          `json:"time"`
				Agent  string          `json:"agent"`
				Event  string          `json:"event"`
				Symbol string          `json:"symbol"`
				Note   string          `json:"note"`
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || !dipRiseAgents[rec.Agent] {
				continue
			}
			note := rec.Note
			// Agent 2 decisions carry the verdict in the structured output, not the note.
			if rec.Agent == "agent2_entry" && rec.Event == "decision" {
				var d struct {
					Action     string  `json:"action"`
					Confidence float64 `json:"confidence"`
					Reason     string  `json:"reason"`
				}
				if json.Unmarshal(rec.Output, &d) == nil && d.Action != "" {
					note = fmt.Sprintf("%s (%.2f) — %s", strings.ToUpper(d.Action), d.Confidence, d.Reason)
				}
			}
			out = append(out, DipRiseEvent{Time: rec.Time, Agent: rec.Agent, Event: rec.Event,
				Symbol: rec.Symbol, Note: note})
		}
	}
	// Newest first, capped.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
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
