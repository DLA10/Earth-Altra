package signals

import (
	"math"
	"sort"
)

// The ML gate (QUANT_VISION Phase 2, v1 baseline): a per-strategy ridge regression that
// predicts a signal's expected R-multiple from its feature snapshot, trained on resolved
// counterfactual outcomes. A signal trades only when the predicted R clears a margin.
//
// Deliberately the SIMPLEST defensible model: closed-form, deterministic, dependency-free
// Go — so the exact same code gates the backtest, the shadow engine, and (later) paper
// execution. Heavier models (LightGBM in the Python sidecar) are the upgrade path and
// must beat this baseline out-of-sample before they ship.

// GateModel is one strategy's trained predictor.
type GateModel struct {
	Strategy string    `json:"strategy"`
	Keys     []string  `json:"keys"` // ordered feature names
	Mean     []float64 `json:"mean"` // per-feature standardization
	Std      []float64 `json:"std"`
	W        []float64 `json:"w"` // len(Keys)+1; W[0] = bias
	TrainN   int       `json:"train_n"`
}

// Predict returns the expected R-multiple for a feature snapshot.
func (m *GateModel) Predict(features map[string]float64) float64 {
	out := m.W[0]
	for i, k := range m.Keys {
		v := features[k]
		if m.Std[i] > 0 {
			v = (v - m.Mean[i]) / m.Std[i]
		} else {
			v = 0
		}
		out += m.W[i+1] * v
	}
	return out
}

// Gate holds the per-strategy models and the trading rule.
type Gate struct {
	Margin  float64 // minimum predicted R to trade (small positive edge demand)
	MinRows int     // rows required before a strategy's model activates
	Lambda  float64 // ridge regularization strength
	models  map[string]*GateModel
}

// NewGate returns a gate with the Phase-2 v1 defaults.
func NewGate() *Gate {
	return &Gate{Margin: 0.03, MinRows: 150, Lambda: 1.0, models: map[string]*GateModel{}}
}

// Train (re)fits one model per strategy from resolved signal outcomes. Call it with rows
// from STRICTLY BEFORE the period being gated — the walk-forward discipline lives with
// the caller.
func (g *Gate) Train(rows []DatasetRow) {
	byStrat := map[string][]DatasetRow{}
	for _, r := range rows {
		byStrat[r.Strategy] = append(byStrat[r.Strategy], r)
	}
	for strat, rs := range byStrat {
		if len(rs) < g.MinRows {
			continue
		}
		if m := trainRidge(rs, strat, g.Lambda); m != nil {
			g.models[strat] = m
		}
	}
}

// Allow scores a signal. applied=false means no trained model exists yet for this
// strategy (warmup) — the caller decides pass-through vs block; ok is meaningful only
// when applied.
func (g *Gate) Allow(sig Signal) (ok bool, predR float64, applied bool) {
	m := g.models[sig.Strategy]
	if m == nil {
		return false, 0, false
	}
	predR = m.Predict(sig.Features)
	return predR >= g.Margin, predR, true
}

// Models exposes the trained models (observability / persistence).
func (g *Gate) Models() map[string]*GateModel { return g.models }

// trainRidge fits standardized ridge regression of r_multiple on the features:
// (XᵀX + λI)w = Xᵀy with an unregularized bias term.
func trainRidge(rows []DatasetRow, strategy string, lambda float64) *GateModel {
	// Stable feature order: the sorted union of keys across the training rows.
	keySet := map[string]bool{}
	for _, r := range rows {
		for k := range r.Features {
			keySet[k] = true
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := len(keys)
	if d == 0 {
		return nil
	}
	n := len(rows)

	// Standardization stats.
	mean := make([]float64, d)
	std := make([]float64, d)
	for i, k := range keys {
		var sum float64
		for _, r := range rows {
			sum += r.Features[k]
		}
		mean[i] = sum / float64(n)
		var v float64
		for _, r := range rows {
			dv := r.Features[k] - mean[i]
			v += dv * dv
		}
		std[i] = math.Sqrt(v / float64(n))
	}

	// Design row: [1, z1..zd].
	x := func(r DatasetRow) []float64 {
		out := make([]float64, d+1)
		out[0] = 1
		for i, k := range keys {
			if std[i] > 0 {
				out[i+1] = (r.Features[k] - mean[i]) / std[i]
			}
		}
		return out
	}

	// Normal equations.
	dim := d + 1
	xtx := make([][]float64, dim)
	for i := range xtx {
		xtx[i] = make([]float64, dim)
	}
	xty := make([]float64, dim)
	for _, r := range rows {
		xi := x(r)
		for i := 0; i < dim; i++ {
			for j := 0; j < dim; j++ {
				xtx[i][j] += xi[i] * xi[j]
			}
			xty[i] += xi[i] * r.RMultiple
		}
	}
	for i := 1; i < dim; i++ { // regularize weights, not the bias
		xtx[i][i] += lambda
	}

	w := solve(xtx, xty)
	if w == nil {
		return nil
	}
	return &GateModel{Strategy: strategy, Keys: keys, Mean: mean, Std: std, W: w, TrainN: n}
}

// solve does Gaussian elimination with partial pivoting; nil on a singular system.
func solve(a [][]float64, b []float64) []float64 {
	n := len(b)
	// Work on copies.
	m := make([][]float64, n)
	for i := range m {
		m[i] = append([]float64(nil), a[i]...)
		m[i] = append(m[i], b[i])
	}
	for col := 0; col < n; col++ {
		piv := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[piv][col]) {
				piv = r
			}
		}
		if math.Abs(m[piv][col]) < 1e-12 {
			return nil
		}
		m[col], m[piv] = m[piv], m[col]
		for r := col + 1; r < n; r++ {
			f := m[r][col] / m[col][col]
			for c := col; c <= n; c++ {
				m[r][c] -= f * m[col][c]
			}
		}
	}
	w := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := m[i][n]
		for j := i + 1; j < n; j++ {
			s -= m[i][j] * w[j]
		}
		w[i] = s / m[i][i]
	}
	return w
}
