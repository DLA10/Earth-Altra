package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Review is the structured post-market report (recorded daily). It measures CONSISTENCY and
// proposes evidence-backed tweaks for the user to approve via the pre-market session.
type Review struct {
	Date             string   `json:"date"`
	GeneratedAt      string   `json:"generated_at"`
	Summary          string   `json:"summary"`
	RealizedPNL      float64  `json:"realized_pnl"`
	WinRate          float64  `json:"win_rate"`
	TotalTrades      int      `json:"total_trades"`
	WhatWorked       []string `json:"what_worked"`
	WhatDidnt        []string `json:"what_didnt"`
	SuggestedChanges []string `json:"suggested_changes"`
	ConsistencyScore float64  `json:"consistency_score"` // 0..10 self-assessment
}

// Reviewer generates the daily review from the decision log + the day's reconstructed trades.
type Reviewer struct {
	client  *Anthropic
	model   string
	log     *DecisionLog
	stateFn func() QuantState
	dir     string
	loc     *time.Location
	system  string
}

func NewReviewer(client *Anthropic, model string, dlog *DecisionLog, stateFn func() QuantState, dataDir string, loc *time.Location) *Reviewer {
	if strings.TrimSpace(model) == "" {
		model = "claude-opus-4-8"
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Reviewer{client: client, model: model, log: dlog, stateFn: stateFn,
		dir: filepath.Join(dataDir, "reviews"), loc: loc, system: reviewSystem()}
}

func (r *Reviewer) Enabled() bool { return r.client != nil && r.client.Enabled() && r.log != nil }

// RunDaily generates the review once each weekday shortly after the close (16:10 ET).
func (r *Reviewer) RunDaily(ctx context.Context) {
	if !r.Enabled() {
		return
	}
	var lastDay string
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().In(r.loc)
			day := now.Format("2006-01-02")
			weekday := now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
			afterClose := now.Hour() > 16 || (now.Hour() == 16 && now.Minute() >= 10)
			if weekday && afterClose && day != lastDay {
				if _, err := r.Generate(day); err != nil {
					// leave lastDay unset so it retries on the next tick
					continue
				}
				lastDay = day
			}
		}
	}
}

var reviewSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"summary":           map[string]interface{}{"type": "string", "description": "2-3 sentence plain-English recap of the day"},
		"what_worked":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		"what_didnt":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		"suggested_changes": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "evidence-backed parameter/rule tweaks (for user approval)"},
		"consistency_score": map[string]interface{}{"type": "number", "description": "0..10 — how consistent/disciplined the day was"},
	},
	"required":             []string{"summary", "what_worked", "what_didnt", "suggested_changes", "consistency_score"},
	"additionalProperties": false,
}

// Generate reads the day's log + trades, asks Opus for a structured review, and writes it.
func (r *Reviewer) Generate(day string) (Review, error) {
	records, _ := r.log.ReadDay(day)
	var state QuantState
	if r.stateFn != nil {
		state = r.stateFn()
	}
	input := r.buildInput(day, records, state)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	raw, usage, err := r.client.Call(ctx, r.model, r.system,
		"record_review", "Record the structured post-market review.", reviewSchema, input, 1200)
	if err != nil {
		return Review{}, err
	}
	var rv Review
	if err := json.Unmarshal(raw, &rv); err != nil {
		return Review{}, err
	}
	rv.Date = day
	rv.GeneratedAt = time.Now().In(r.loc).Format("15:04 MST")
	rv.RealizedPNL = round2(state.RealizedPNL)
	rv.WinRate = round2(state.WinRate)
	rv.TotalTrades = state.TotalTrades

	if err := os.MkdirAll(r.dir, 0o755); err == nil {
		if b, e := json.MarshalIndent(rv, "", "  "); e == nil {
			_ = os.WriteFile(filepath.Join(r.dir, day+".json"), b, 0o644)
		}
	}
	r.log.Append(LogRecord{Agent: "review", Event: "decision", Model: r.model, Output: rv, Tokens: &usage,
		Note: fmt.Sprintf("daily review for %s", day)})
	return rv, nil
}

// buildInput compiles a compact, token-efficient digest of the day for the reviewer.
func (r *Reviewer) buildInput(day string, records []LogRecord, state QuantState) string {
	var entries, noBuys, errors int
	type dec struct {
		Symbol     string  `json:"symbol"`
		Action     string  `json:"action"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	var agent2 []dec
	for _, rec := range records {
		if rec.Agent == "agent2_entry" && rec.Event == "decision" {
			var ed EntryDecision
			if rec.Output != nil {
				b, _ := json.Marshal(rec.Output)
				_ = json.Unmarshal(b, &ed)
			}
			if ed.IsBuy() {
				entries++
			} else {
				noBuys++
			}
			agent2 = append(agent2, dec{Symbol: rec.Symbol, Action: ed.Action, Confidence: ed.Confidence, Reason: ed.Reason})
		}
		if rec.Event == "error" {
			errors++
		}
	}
	digest := map[string]interface{}{
		"date":             day,
		"realized_pnl":     round2(state.RealizedPNL),
		"win_rate":         round2(state.WinRate),
		"total_trades":     state.TotalTrades,
		"open_positions":   len(state.Positions),
		"entries_taken":    entries,
		"entries_passed":   noBuys,
		"errors":           errors,
		"closed_trades":    state.Trades,
		"agent2_decisions": agent2,
	}
	b, _ := json.Marshal(digest)
	return string(b)
}

func reviewSystem() string {
	return Constitution + "\n\n" + reviewPlaybook
}

const reviewPlaybook = `YOUR ROLE: You are the post-market REVIEW analyst for the quant team. After the close you receive
a structured digest of the day: realized P&L (realized-only), win rate, closed trades (with exit
reasons), how many dips were entered vs passed, and Agent 2's decisions. Produce a concise,
HONEST, structured review whose purpose is to push the team toward CONSISTENT profitability.

Judge the day by CONSISTENCY and DISCIPLINE, not by whether it happened to make money:
- Did entries cluster on high-quality setups, or did it chase low-conviction dips?
- Were losers cut appropriately, winners managed without giving back gains?
- Was the selectivity right (passing most dips is GOOD)?
- Any errors, rule violations, or erratic behavior?

Then in suggested_changes, propose at most 2-3 SPECIFIC, EVIDENCE-BACKED tweaks (e.g. "3 of 4
losers were sub-1.3 RVOL — raise the entry RVOL bar", "stops triggered then price recovered on 2
trades — widen the trailing stop from 1.5% to 2%"). Each must cite the evidence in the digest.
Propose nothing if the day doesn't justify a change — stability is part of consistency. These are
suggestions for the human to approve, not auto-applied.

consistency_score (0..10): how disciplined/repeatable the day was (10 = textbook discipline).
Keep summary to 2-3 plain sentences. Call record_review.`
