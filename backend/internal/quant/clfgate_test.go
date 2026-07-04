package quant

import (
	"testing"

	"live-optimus/backend/internal/signals"
)

// TestClfGateParityAgainstTrainedModels is the train/serve-skew guard run against the
// REAL nightly artifacts: NewClfGate refuses any model whose Go-side prediction
// pipeline (vectorize -> leaves trees -> sigmoid) fails to reproduce the Python
// trainer's own probabilities, so Ready()==true here proves numeric parity end to end.
// Skips when no trained models exist (fresh clone).
func TestClfGateParityAgainstTrainedModels(t *testing.T) {
	g := NewClfGate("../../data/models")
	if g.LastDay() == "" && !g.Ready() {
		t.Skip("no trained models on disk (run ml/train_live.py first)")
	}
	if !g.Ready() {
		t.Fatal("models exist on disk but none passed parity/staleness verification — see [clf-gate] logs")
	}
	if m := g.Margin(); m != 0.03 {
		t.Fatalf("margin = %v, want the pre-registered 0.03", m)
	}
	// One live-shaped scoring call: a vwap_reclaim signal with a 1.5R bracket must come
	// back scored, with a finite EV in a sane range.
	sig := signals.Signal{
		Strategy:  "vwap_reclaim",
		Features:  map[string]float64{"atr": 2.5, "rvol": 1.4, "minute": 90, "market_ok": 1},
		Suggested: signals.Suggested{Entry: 100, Stop: 98, Target: 103},
	}
	ev, scored := g.Score(sig)
	if !scored {
		t.Fatal("vwap_reclaim should have a verified model")
	}
	if ev < -1 || ev > 1.5 {
		t.Fatalf("EV %v outside the possible range [-1, rr]", ev)
	}
}

func TestClfGateFailsOpenWithoutModels(t *testing.T) {
	g := NewClfGate(t.TempDir())
	if g.Ready() {
		t.Fatal("empty model dir must leave the gate disabled")
	}
	if _, scored := g.Score(signals.Signal{Strategy: "vwap_reclaim"}); scored {
		t.Fatal("disabled gate must not score (fail-open pass-through)")
	}
}
