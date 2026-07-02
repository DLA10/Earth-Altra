package quant

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// QuantReport is the full state the Paper·Claude page renders.
type QuantReport struct {
	Live         bool            `json:"live"`
	UniverseSize int             `json:"universe_size"`
	Posture      string          `json:"posture"`
	Alloc        AllocSnapshot   `json:"alloc"`
	State        QuantState      `json:"state"`
	Attribution  ExitAttribution `json:"attribution"`
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
	r.Review = e.latestReview()
	return r
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
