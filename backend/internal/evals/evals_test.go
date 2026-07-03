package evals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSONL(t *testing.T, path string, lines []interface{}) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// TestComputeDemotionAndJudgeJoin fabricates two signal-journal files and one decisions
// file to exercise the three load-bearing behaviors of Compute: per-strategy signal/
// outcome/traded counts, the negative-expectancy demotion rule, and the judge-to-outcome
// join by signal_id.
func TestComputeDemotionAndJudgeJoin(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "signals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}

	day1 := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	day2 := time.Now().Format("2006-01-02")

	// File 1: 5 signals + 20 negative outcomes for "test_strat".
	var f1 []interface{}
	for i := 0; i < 5; i++ {
		var sr signalRec
		sr.Type = "signal"
		sr.Signal.ID = fmt.Sprintf("sig-%d", i)
		sr.Signal.Strategy = "test_strat"
		f1 = append(f1, sr)
	}
	for i := 0; i < 20; i++ {
		f1 = append(f1, outcomeRec{Type: "outcome", ID: fmt.Sprintf("out-%d", i), Strategy: "test_strat", R: -0.1})
	}
	writeJSONL(t, filepath.Join(dir, "signals", day1+".jsonl"), f1)

	// File 2: 10 more negative outcomes for "test_strat" (crosses demoteMinOutcomes=30),
	// a "healthy_strat" with too few outcomes to be judged, and one outcome ("joinme")
	// used to verify the judge-join-by-signal_id path.
	var f2 []interface{}
	for i := 20; i < 30; i++ {
		f2 = append(f2, outcomeRec{Type: "outcome", ID: fmt.Sprintf("out-%d", i), Strategy: "test_strat", R: -0.1})
	}
	for i := 0; i < 5; i++ {
		f2 = append(f2, outcomeRec{Type: "outcome", ID: fmt.Sprintf("healthy-%d", i), Strategy: "healthy_strat", R: 0.3})
	}
	f2 = append(f2, outcomeRec{Type: "outcome", ID: "joinme", Strategy: "judge_strat", R: 0.5})
	writeJSONL(t, filepath.Join(dir, "signals", day2+".jsonl"), f2)

	// Decisions: two signal_trader orders for test_strat (traded count) and a
	// signal_judge decision joined to the "joinme" outcome by signal_id.
	dec := []interface{}{
		map[string]interface{}{"agent": "signal_trader", "event": "order", "note": "test_strat: funded $500 (conf 0.60)"},
		map[string]interface{}{"agent": "signal_trader", "event": "order", "note": "test_strat: funded $500 (conf 0.65)"},
		map[string]interface{}{
			"agent": "signal_judge", "event": "decision",
			"input":  json.RawMessage(`{"signal_id":"joinme"}`),
			"output": map[string]interface{}{"action": "buy", "confidence": 0.9},
		},
	}
	writeJSONL(t, filepath.Join(dir, "decisions", day2+".jsonl"), dec)

	sb, err := Compute(dir, 20, time.UTC)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	var testStrat, healthyStrat *StrategyRow
	for i := range sb.Strategies {
		switch sb.Strategies[i].Strategy {
		case "test_strat":
			testStrat = &sb.Strategies[i]
		case "healthy_strat":
			healthyStrat = &sb.Strategies[i]
		}
	}
	if testStrat == nil {
		t.Fatal("test_strat missing from scoreboard")
	}
	if testStrat.Signals != 5 {
		t.Errorf("signals = %d, want 5", testStrat.Signals)
	}
	if testStrat.Outcomes != 30 {
		t.Errorf("outcomes = %d, want 30", testStrat.Outcomes)
	}
	if testStrat.Traded != 2 {
		t.Errorf("traded = %d, want 2", testStrat.Traded)
	}
	if !testStrat.Demoted || testStrat.Reason != "negative rolling expectancy" {
		t.Errorf("expected negative-expectancy demotion, got demoted=%v reason=%q", testStrat.Demoted, testStrat.Reason)
	}
	if !sb.IsDemoted("test_strat") {
		t.Error("IsDemoted(test_strat) = false")
	}

	if healthyStrat == nil {
		t.Fatal("healthy_strat missing from scoreboard")
	}
	if healthyStrat.Demoted {
		t.Error("healthy_strat should not be demoted (below demoteMinOutcomes)")
	}

	if sb.Judge.Decisions != 1 {
		t.Errorf("judge decisions = %d, want 1", sb.Judge.Decisions)
	}
	if sb.Judge.Joined != 1 {
		t.Errorf("judge joined = %d, want 1", sb.Judge.Joined)
	}
	if sb.Judge.ApprovedMeanR != 0.5 {
		t.Errorf("approved mean R = %v, want 0.5", sb.Judge.ApprovedMeanR)
	}
}
