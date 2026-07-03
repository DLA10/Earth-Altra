// Package evals is the scoreboard + agent-calibration layer (QUANT_VISION §5): it reads
// the signal journals and decision logs, computes per-strategy rolling counterfactual
// expectancy, a CUSUM changepoint watchdog, and LLM-judge calibration (Brier score +
// veto value), and decides which strategies are DEMOTED to shadow. Pure measurement —
// paper pipeline only; it never touches the live order path.
//
// Pre-registered rules (fixed before any data was scored; never swept):
//   - demotion: ≥ demoteMinOutcomes resolved counterfactual outcomes in the window AND
//     mean R < 0  →  demoted
//   - CUSUM watchdog: one-sided negative-shift CUSUM on the outcome-R stream,
//     s = max(0, s + (−r − cusumSlack)); alarm at s > cusumThreshold  →  demoted
package evals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	demoteMinOutcomes = 30
	cusumSlack        = 0.05
	cusumThreshold    = 3.0
)

// StrategyRow is one strategy's scoreboard line.
type StrategyRow struct {
	Strategy   string  `json:"strategy"`
	Signals    int     `json:"signals"`  // published in window
	Outcomes   int     `json:"outcomes"` // resolved counterfactuals
	MeanR      float64 `json:"mean_r"`   // counterfactual expectancy (R units)
	Traded     int     `json:"traded"`   // real paper entries in window
	CusumAlarm bool    `json:"cusum_alarm"`
	Demoted    bool    `json:"demoted"`
	Reason     string  `json:"reason,omitempty"`
}

// JudgeCalib scores the LLM entry judge against counterfactual reality.
type JudgeCalib struct {
	Decisions     int     `json:"decisions"`
	Approved      int     `json:"approved"`
	Vetoed        int     `json:"vetoed"`
	Joined        int     `json:"joined"` // decisions matched to an outcome by signal id
	ApprovedMeanR float64 `json:"approved_mean_r"`
	VetoedMeanR   float64 `json:"vetoed_mean_r"`
	VetoValueR    float64 `json:"veto_value_r"` // approved − vetoed; positive = vetoes help
	Brier         float64 `json:"brier"`        // confidence vs (r>0), joined approved+vetoed
}

// Scoreboard is the persisted/served eval snapshot.
type Scoreboard struct {
	GeneratedAt time.Time     `json:"generated_at"`
	WindowDays  int           `json:"window_days"`
	Strategies  []StrategyRow `json:"strategies"`
	Judge       JudgeCalib    `json:"judge"`
	DemotedSet  []string      `json:"demoted_set"`
}

// IsDemoted reports whether a strategy is currently benched.
func (s *Scoreboard) IsDemoted(strategy string) bool {
	for _, d := range s.DemotedSet {
		if d == strategy {
			return true
		}
	}
	return false
}

type outcomeRec struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	Strategy string  `json:"strategy"`
	R        float64 `json:"r_multiple"`
	ExitTime int64   `json:"exit_time"`
}

type signalRec struct {
	Type   string `json:"type"`
	Signal struct {
		ID       string `json:"id"`
		Strategy string `json:"strategy"`
	} `json:"signal"`
}

type decisionRec struct {
	Agent  string          `json:"agent"`
	Event  string          `json:"event"`
	Note   string          `json:"note"`
	Input  json.RawMessage `json:"input"`
	Output struct {
		Action     string  `json:"action"`
		Confidence float64 `json:"confidence"`
	} `json:"output"`
}

// Compute builds the scoreboard from the last windowDays trading days of journals.
func Compute(dataDir string, windowDays int, loc *time.Location) (*Scoreboard, error) {
	if loc == nil {
		loc = time.UTC
	}
	cutoff := time.Now().In(loc).AddDate(0, 0, -(windowDays*7/5 + 4)).Format("2006-01-02")

	type stratAgg struct {
		signals, outcomes, traded int
		sumR                      float64
		rs                        []float64 // chronological, for CUSUM
	}
	agg := map[string]*stratAgg{}
	get := func(s string) *stratAgg {
		if agg[s] == nil {
			agg[s] = &stratAgg{}
		}
		return agg[s]
	}
	outcomeByID := map[string]float64{}

	// Signal journals (chronological by filename).
	sigFiles, _ := filepath.Glob(filepath.Join(dataDir, "signals", "*.jsonl"))
	sort.Strings(sigFiles)
	for _, f := range sigFiles {
		day := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if day < cutoff {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line == "" {
				continue
			}
			switch {
			case strings.Contains(line, `"type":"signal"`):
				var r signalRec
				if json.Unmarshal([]byte(line), &r) == nil && r.Signal.Strategy != "" {
					get(r.Signal.Strategy).signals++
				}
			case strings.Contains(line, `"type":"outcome"`):
				var r outcomeRec
				if json.Unmarshal([]byte(line), &r) == nil && r.Strategy != "" {
					a := get(r.Strategy)
					a.outcomes++
					a.sumR += r.R
					a.rs = append(a.rs, r.R)
					if r.ID != "" {
						outcomeByID[r.ID] = r.R
					}
				}
			}
		}
	}

	// Decision logs: traded counts + judge decisions.
	judge := JudgeCalib{}
	type judged struct {
		id, action string
		conf       float64
	}
	var judgeRows []judged
	decFiles, _ := filepath.Glob(filepath.Join(dataDir, "decisions", "*.jsonl"))
	sort.Strings(decFiles)
	for _, f := range decFiles {
		day := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if day < cutoff {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line == "" {
				continue
			}
			var r decisionRec
			if json.Unmarshal([]byte(line), &r) != nil {
				continue
			}
			switch {
			case r.Agent == "signal_trader" && r.Event == "order":
				// Note format: "<strategy>: funded $N (conf x)".
				if i := strings.Index(r.Note, ":"); i > 0 {
					get(r.Note[:i]).traded++
				}
			case r.Agent == "signal_judge" && r.Event == "decision":
				judge.Decisions++
				if r.Output.Action == "buy" {
					judge.Approved++
				} else {
					judge.Vetoed++
				}
				var in struct {
					SignalID string `json:"signal_id"`
				}
				_ = json.Unmarshal(r.Input, &in)
				judgeRows = append(judgeRows, judged{id: in.SignalID, action: r.Output.Action, conf: r.Output.Confidence})
			}
		}
	}

	// Judge calibration against counterfactual outcomes.
	var apSum, veSum, brier float64
	var apN, veN int
	for _, j := range judgeRows {
		r, ok := outcomeByID[j.id]
		if !ok || j.id == "" {
			continue
		}
		judge.Joined++
		win := 0.0
		if r > 0 {
			win = 1
		}
		brier += (j.conf - win) * (j.conf - win)
		if j.action == "buy" {
			apSum += r
			apN++
		} else {
			veSum += r
			veN++
		}
	}
	if apN > 0 {
		judge.ApprovedMeanR = apSum / float64(apN)
	}
	if veN > 0 {
		judge.VetoedMeanR = veSum / float64(veN)
	}
	if apN > 0 && veN > 0 {
		judge.VetoValueR = judge.ApprovedMeanR - judge.VetoedMeanR
	}
	if judge.Joined > 0 {
		judge.Brier = brier / float64(judge.Joined)
	}

	sb := &Scoreboard{GeneratedAt: time.Now().In(loc), WindowDays: windowDays}
	names := make([]string, 0, len(agg))
	for s := range agg {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		a := agg[s]
		row := StrategyRow{Strategy: s, Signals: a.signals, Outcomes: a.outcomes, Traded: a.traded}
		if a.outcomes > 0 {
			row.MeanR = a.sumR / float64(a.outcomes)
		}
		// CUSUM watchdog on the chronological outcome stream.
		cs := 0.0
		for _, r := range a.rs {
			cs += -r - cusumSlack
			if cs < 0 {
				cs = 0
			}
			if cs > cusumThreshold {
				row.CusumAlarm = true
			}
		}
		switch {
		case a.outcomes >= demoteMinOutcomes && row.MeanR < 0:
			row.Demoted, row.Reason = true, "negative rolling expectancy"
		case row.CusumAlarm:
			row.Demoted, row.Reason = true, "CUSUM changepoint alarm"
		}
		if row.Demoted {
			sb.DemotedSet = append(sb.DemotedSet, s)
		}
		sb.Strategies = append(sb.Strategies, row)
	}
	sb.Judge = judge
	return sb, nil
}

// Save persists the scoreboard to <dataDir>/evals/scoreboard.json (best-effort).
func (s *Scoreboard) Save(dataDir string) {
	dir := filepath.Join(dataDir, "evals")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	if b, err := json.MarshalIndent(s, "", " "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "scoreboard.json"), b, 0o644)
	}
}
