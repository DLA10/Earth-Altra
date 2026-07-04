package quant

import (
	"sort"
	"sync"
)

// Allocator is the deterministic shared-budget manager — the "portfolio manager" of the quant
// team. It is NOT an LLM: capital allocation under contention must be fast, consistent, and
// auditable, so it's pure rules. It tracks one shared budget across all positions, enforces a
// max-concurrent cap, sizes by conviction (full/half slice), ranks competing approved buys, and
// recycles capital live as positions close.
type Allocator struct {
	mu          sync.Mutex
	budget      float64 // configured target budget (from config / daily_universe)
	perPosition float64
	maxConc     int
	deployed    map[string]float64 // symbol -> $ committed (live; freed on Release)
	// equityCeiling is the paper account's REAL equity, synced from the broker. The
	// effective budget is capped at this so the desk can never try to deploy more cash
	// than the account actually holds (0 = not yet known → no cap, assume the budget).
	equityCeiling float64
}

// minEntryConfidence is the hard floor below which no buy is ever funded.
const minEntryConfidence = 0.5

func NewAllocator() *Allocator {
	// Budget matches the quant paper account's cash ($8k) so the team allocates real, not
	// margin, capital. With a $2k per-position slice this funds up to 3 full positions
	// with headroom. daily_universe.json's allocation block can override all three knobs.
	return &Allocator{deployed: map[string]float64{}, perPosition: 2000, maxConc: 3, budget: 8000}
}

// Configure applies today's allocation parameters (from the daily universe). Existing deployed
// capital is preserved (so a mid-session reload doesn't lose track of open positions).
func (a *Allocator) Configure(al Allocation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if al.PerPositionUSD > 0 {
		a.perPosition = al.PerPositionUSD
	}
	if al.MaxConcurrent > 0 {
		a.maxConc = al.MaxConcurrent
	}
	if al.BudgetUSD > 0 {
		a.budget = al.BudgetUSD
	}
}

func (a *Allocator) deployedTotalLocked() float64 {
	var t float64
	for _, v := range a.deployed {
		t += v
	}
	return t
}

// effectiveBudgetLocked is the smaller of the configured budget and the account's real
// equity — the desk never commits more than the paper account actually holds.
func (a *Allocator) effectiveBudgetLocked() float64 {
	if a.equityCeiling > 0 && a.equityCeiling < a.budget {
		return a.equityCeiling
	}
	return a.budget
}

func (a *Allocator) freeLocked() float64 {
	f := a.effectiveBudgetLocked() - a.deployedTotalLocked()
	if f < 0 {
		f = 0
	}
	return f
}

// SetEquityCeiling syncs the allocator to the paper account's real equity (from the
// broker). Called at startup and periodically; the effective budget is capped here so a
// drawdown that shrinks the account automatically shrinks what the desk will deploy.
func (a *Allocator) SetEquityCeiling(equity float64) {
	a.mu.Lock()
	a.equityCeiling = equity
	a.mu.Unlock()
}

// halfSlice is the smallest position we'll open (medium-conviction size).
func (a *Allocator) halfSliceLocked() float64 { return a.perPosition * 0.5 }

// CanFund reports whether a NEW position in sym could be opened right now (a free slot, not
// already held, and at least a half-slice of capital free).
func (a *Allocator) CanFund(sym string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, held := a.deployed[sym]; held {
		return false
	}
	if len(a.deployed) >= a.maxConc {
		return false
	}
	return a.freeLocked() >= a.halfSliceLocked()-1e-9
}

// Size returns the dollar size to deploy for a buy at the given conviction, or 0 if it can't be
// meaningfully funded. High conviction (>=0.7) = full slice; otherwise = half slice. Never
// exceeds free capital; if less than a half-slice is free, returns 0.
func (a *Allocator) Size(confidence float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Hard minimum-conviction floor (deterministic): below this we never commit capital,
	// regardless of what the model returns. Consistency > catching every bounce.
	if confidence < minEntryConfidence {
		return 0
	}
	want := a.perPosition
	if confidence < 0.7 {
		want = a.halfSliceLocked()
	}
	free := a.freeLocked()
	if want > free {
		want = free
	}
	if want < a.halfSliceLocked()-1e-9 {
		return 0
	}
	return want
}

// Fund records committed capital for a newly opened position. Returns false if a slot/capital is
// no longer available (caller should skip). Idempotent-safe: re-funding a held symbol is rejected.
func (a *Allocator) Fund(sym string, dollars float64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, held := a.deployed[sym]; held {
		return false
	}
	if len(a.deployed) >= a.maxConc {
		return false
	}
	if dollars <= 0 || dollars > a.freeLocked()+1e-9 {
		return false
	}
	a.deployed[sym] = dollars
	return true
}

// Release returns a position's capital to the free pool when it closes (live recycling).
func (a *Allocator) Release(sym string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.deployed, sym)
}

// Free returns currently available capital.
func (a *Allocator) Free() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.freeLocked()
}

// OpenCount returns the number of open positions.
func (a *Allocator) OpenCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.deployed)
}

// Held reports whether the allocator is tracking capital for sym.
func (a *Allocator) Held(sym string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.deployed[sym]
	return ok
}

// Candidate is one Agent-2-APPROVED buy competing for capital. Only approved buys reach the
// allocator (rejected dips never get here).
type Candidate struct {
	Symbol     string
	Confidence float64 // Agent 2's conviction (0..1)
	Dip        DipEvent
	Tier       int     // universe tier (1 = core)
	Sentiment  float64 // -1..+1 from Agent 4 (0 if unknown)
}

// Score ranks competing approved buys. Base = Agent 2 confidence, lifted by deterministic
// quality factors: how "in play" (RVOL), how meaningful the dip (depth in ATR), universe tier,
// and sentiment. Weights are intentionally modest and are tuned via the review loop, never by an
// LLM at runtime.
func Score(c Candidate) float64 {
	s := c.Confidence
	s *= 1 + clamp(c.Dip.RVOL-1, 0, 2)*0.25       // more relative volume = more trustworthy
	s *= 1 + clamp(c.Dip.DepthATR-0.5, 0, 2)*0.15 // deeper, meaningful pullback
	if c.Tier == 1 {
		s *= 1.10 // core names get a small edge
	}
	s *= 1 + clampSym(c.Sentiment, 0.5)*0.10 // positive sentiment nudges up, negative down
	return s
}

// Rank sorts competing candidates best-first (highest Score). Used only under contention —
// multiple approved buys arriving close together with limited slots/capital.
func Rank(cands []Candidate) []Candidate {
	out := append([]Candidate(nil), cands...)
	sort.SliceStable(out, func(i, j int) bool { return Score(out[i]) > Score(out[j]) })
	return out
}

// AllocSnapshot is a read-only view for the API/page.
type AllocSnapshot struct {
	Budget        float64            `json:"budget"`         // effective (min of configured & equity)
	ConfiguredMax float64            `json:"configured_max"` // the target budget before the equity cap
	AccountEquity float64            `json:"account_equity"` // real paper-account equity (0 = unknown)
	Free          float64            `json:"free"`
	Deployed      float64            `json:"deployed"`
	OpenCount     int                `json:"open_count"`
	MaxConc       int                `json:"max_concurrent"`
	PerPosition   float64            `json:"per_position"`
	Positions     map[string]float64 `json:"positions"`
}

func (a *Allocator) Snapshot() AllocSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	pos := make(map[string]float64, len(a.deployed))
	for k, v := range a.deployed {
		pos[k] = v
	}
	return AllocSnapshot{
		Budget: a.effectiveBudgetLocked(), ConfiguredMax: a.budget, AccountEquity: a.equityCeiling,
		Free: a.freeLocked(), Deployed: a.deployedTotalLocked(),
		OpenCount: len(a.deployed), MaxConc: a.maxConc, PerPosition: a.perPosition, Positions: pos,
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampSym clamps v to [-mag, +mag].
func clampSym(v, mag float64) float64 { return clamp(v, -mag, mag) }
