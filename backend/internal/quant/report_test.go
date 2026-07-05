package quant

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDipScorecard fabricates a day of decision-log records and checks the dip scorecard
// computes Agent 2's approve/reject counts and the per-pipeline realized outcomes — the
// measurement the operator relies on to judge whether the dip agent picks real bounces or
// catches falling knives.
func TestDipScorecard(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		// Agent 2 entry decisions: 3 buys, 2 no-buys.
		`{"agent":"agent2_entry","event":"decision","output":{"action":"buy","confidence":0.7}}`,
		`{"agent":"agent2_entry","event":"decision","output":{"action":"buy","confidence":0.6}}`,
		`{"agent":"agent2_entry","event":"decision","output":{"action":"buy","confidence":0.8}}`,
		`{"agent":"agent2_entry","event":"decision","output":{"action":"no_buy","confidence":0.3}}`,
		`{"agent":"agent2_entry","event":"decision","output":{"action":"no_buy","confidence":0.2}}`,
		// Dip-pipeline outcomes: 1 win (+30), 2 losses (−10, −12) → knife rate 2/3.
		`{"agent":"pipeline","event":"outcome","symbol":"AAA","output":{"source":"dip","pnl":30,"win":true}}`,
		`{"agent":"pipeline","event":"outcome","symbol":"BBB","output":{"source":"dip","pnl":-10,"win":false}}`,
		`{"agent":"pipeline","event":"outcome","symbol":"CCC","output":{"source":"dip","pnl":-12,"win":false}}`,
		// Signal-pipeline outcomes: 2 wins (should not mix into the dip stats).
		`{"agent":"pipeline","event":"outcome","symbol":"DDD","output":{"source":"signal","pnl":15,"win":true}}`,
		`{"agent":"pipeline","event":"outcome","symbol":"EEE","output":{"source":"signal","pnl":5,"win":true}}`,
		// A rehydrated/untagged outcome must be ignored, not miscounted.
		`{"agent":"pipeline","event":"outcome","symbol":"FFF","output":{"source":"rehydrated","pnl":100,"win":true}}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "decisions", "2026-07-05.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{dataDir: dir}
	sc := e.dipScorecard(20)
	if sc == nil {
		t.Fatal("nil scorecard")
	}
	if sc.Decisions != 5 || sc.Approved != 3 || sc.Rejected != 2 {
		t.Errorf("decisions=%d approved=%d rejected=%d, want 5/3/2", sc.Decisions, sc.Approved, sc.Rejected)
	}
	if sc.AvgConf != 0.7 { // (0.7+0.6+0.8)/3
		t.Errorf("avg_confidence=%v, want 0.70", sc.AvgConf)
	}
	if sc.Dip.Trades != 3 || sc.Dip.Wins != 1 {
		t.Errorf("dip trades=%d wins=%d, want 3/1", sc.Dip.Trades, sc.Dip.Wins)
	}
	if sc.Dip.TotalPNL != 8 { // 30 - 10 - 12
		t.Errorf("dip total_pnl=%v, want 8", sc.Dip.TotalPNL)
	}
	if sc.KnifeRate != 0.67 { // 2 losers / 3
		t.Errorf("knife_rate=%v, want 0.67", sc.KnifeRate)
	}
	if sc.Signal.Trades != 2 || sc.Signal.Wins != 2 {
		t.Errorf("signal trades=%d wins=%d, want 2/2 (must not mix with dip)", sc.Signal.Trades, sc.Signal.Wins)
	}
	if sc.Rehydrated.Trades != 1 || sc.Rehydrated.TotalPNL != 100 {
		t.Errorf("rehydrated trades=%d pnl=%v, want 1/100 (tracked separately, not dropped)", sc.Rehydrated.Trades, sc.Rehydrated.TotalPNL)
	}
	if sc.Verdict == "" {
		t.Error("expected a plain-language verdict")
	}
}
