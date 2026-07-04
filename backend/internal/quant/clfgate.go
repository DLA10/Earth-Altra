package quant

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dmitryikh/leaves"

	"live-optimus/backend/internal/signals"
)

// ClfGate scores every published signal with the per-strategy LightGBM classifiers
// trained nightly by ml/train_live.py — the promoted RESEARCH_BACKLOG #15 mechanism
// (the only one to pass the full pre-registered bar: positive accepted-vs-rejected
// spread AND positive dollars on the 12-month, recent-half, and holdout windows).
//
// Score = p(win)*rewardRisk - (1-p)  — expected R using the signal's own bracket —
// accepted at >= the meta margin (pre-registered 0.03).
//
// FAIL-OPEN BY DESIGN: a missing/stale/unparseable model, a failed parity check, or a
// strategy below the training row bar means the signal passes UNGATED — exactly the
// validated "warmup pass-through" semantics, which degrades to the pre-gate baseline
// rather than silently blocking the desk. Paper-side only; nothing here touches the
// real-money path.
//
// PARITY: the meta file carries sample feature maps with the trainer's own predicted
// probabilities. On every load the Go pipeline (vector build -> trees -> sigmoid) must
// reproduce each within parityTol or that model is refused. This is the guard against
// silent train/serve skew (leaves parsing a LightGBM 4.x file subtly wrong, feature
// ordering drift, fill-value drift).
type ClfGate struct {
	dir string

	mu      sync.RWMutex
	models  map[string]*leaves.Ensemble
	keys    map[string][]string // per-strategy feature order (sorted, from meta)
	margin  float64
	lastDay string
	loaded  bool
}

const (
	clfParityTol    = 1e-6
	clfMaxStaleDays = 7 // refuse models whose last training day is older than this
)

type clfMeta struct {
	LastDay    string  `json:"last_day"`
	Margin     float64 `json:"margin"`
	Strategies map[string]struct {
		ModelFile   string   `json:"model_file"`
		Rows        int      `json:"rows"`
		FeatureKeys []string `json:"feature_keys"`
		Parity      []struct {
			Features  map[string]float64 `json:"features"`
			ExpectedP float64            `json:"expected_p"`
		} `json:"parity"`
	} `json:"strategies"`
}

// NewClfGate loads (or fails open) from dir (backend/data/models). Reload is called by
// the nightly retrain scheduler after a successful training run.
func NewClfGate(dir string) *ClfGate {
	g := &ClfGate{dir: dir}
	g.Reload()
	return g
}

// Ready reports whether at least one strategy model is loaded, parity-verified, and
// fresh enough to score with.
func (g *ClfGate) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.loaded
}

// Margin returns the accept threshold recorded by the trainer (0.03 pre-registered).
func (g *ClfGate) Margin() float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.margin
}

// LastDay returns the newest training day of the loaded models ("" when not loaded).
func (g *ClfGate) LastDay() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.lastDay
}

// Reload reads clf_meta.json + models from disk, verifying parity per strategy.
// Any strategy that fails verification is dropped (fail-open for its signals); a fully
// failed load leaves the gate disabled (fail-open for everything).
func (g *ClfGate) Reload() {
	metaPath := filepath.Join(g.dir, "clf_meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		g.disable(fmt.Sprintf("no model meta at %s (gate fail-open)", metaPath))
		return
	}
	var meta clfMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		g.disable(fmt.Sprintf("bad model meta: %v (gate fail-open)", err))
		return
	}
	if d, err := time.Parse("2006-01-02", meta.LastDay); err != nil || time.Since(d) > clfMaxStaleDays*24*time.Hour {
		g.disable(fmt.Sprintf("models stale (last training day %q > %dd old) — retrain needed (gate fail-open)", meta.LastDay, clfMaxStaleDays))
		return
	}
	margin := meta.Margin
	if margin <= 0 || margin > 1 {
		margin = 0.03
	}

	models := map[string]*leaves.Ensemble{}
	keys := map[string][]string{}
	for strat, sm := range meta.Strategies {
		ens, err := leaves.LGEnsembleFromFile(filepath.Join(g.dir, sm.ModelFile), false)
		if err != nil {
			log.Printf("[clf-gate] %s: model load failed (%v) — strategy fails open", strat, err)
			continue
		}
		ok := true
		for i, pr := range sm.Parity {
			p := predictP(ens, vectorize(pr.Features, sm.FeatureKeys))
			if math.Abs(p-pr.ExpectedP) > clfParityTol {
				log.Printf("[clf-gate] %s: PARITY FAILED on sample %d (go=%.9f python=%.9f) — refusing model, strategy fails open",
					strat, i, p, pr.ExpectedP)
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		models[strat] = ens
		keys[strat] = sm.FeatureKeys
	}
	if len(models) == 0 {
		g.disable("no model passed verification (gate fail-open)")
		return
	}

	g.mu.Lock()
	g.models, g.keys, g.margin, g.lastDay, g.loaded = models, keys, margin, meta.LastDay, true
	g.mu.Unlock()
	log.Printf("[clf-gate] loaded %d strategy models (trained through %s, margin %.2f) — parity verified", len(models), meta.LastDay, margin)
}

func (g *ClfGate) disable(reason string) {
	g.mu.Lock()
	g.models, g.keys, g.lastDay, g.loaded = nil, nil, "", false
	g.mu.Unlock()
	log.Printf("[clf-gate] %s", reason)
}

// Score returns the signal's expected R and whether it was scored at all (false =
// no verified model for this strategy -> caller must pass the signal through ungated).
func (g *ClfGate) Score(sig signals.Signal) (float64, bool) {
	g.mu.RLock()
	ens, keys := g.models[sig.Strategy], g.keys[sig.Strategy]
	g.mu.RUnlock()
	if ens == nil {
		return 0, false
	}
	p := predictP(ens, vectorize(sig.Features, keys))
	risk := sig.Suggested.Entry - sig.Suggested.Stop
	rr := 1.0
	if risk > 0 {
		rr = (sig.Suggested.Target - sig.Suggested.Entry) / risk
	}
	return p*rr - (1 - p), true
}

// vectorize builds the model input in the trainer's exact column order; missing keys
// fill 0.0 (train_gate.py/train_live.py semantics — NOT NaN).
func vectorize(features map[string]float64, keys []string) []float64 {
	v := make([]float64, len(keys))
	for i, k := range keys {
		v[i] = features[k] // zero value for missing keys = the trainer's 0.0 fill
	}
	return v
}

// predictP runs the trees and applies the binary-objective sigmoid (LGBMClassifier's
// predict_proba[:, 1]).
func predictP(ens *leaves.Ensemble, v []float64) float64 {
	raw := ens.PredictSingle(v, 0)
	return 1 / (1 + math.Exp(-raw))
}
