package quant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Universe holds today's curated trading set (from backend/data/daily_universe.json, written by
// the pre-market Claude session per Instruction.md). The AGENTS act only on these symbols; the
// broad Telegram dip bot still scans the whole watchlist (one detector, labeled output). If the
// file is missing or empty, the universe is empty → the agent pipeline is idle (safe default).
type Universe struct {
	path string
	mu   sync.RWMutex
	cur  DailyUniverse
	syms map[string]UniverseEntry
}

// NewUniverse loads the daily file from <dataDir>/daily_universe.json (missing file is fine).
func NewUniverse(dataDir string) *Universe {
	u := &Universe{path: filepath.Join(dataDir, "daily_universe.json"), syms: map[string]UniverseEntry{}}
	_ = u.Reload()
	return u
}

// Reload re-reads the daily file (call at startup and when the pre-market session rewrites it).
func (u *Universe) Reload() error {
	b, err := os.ReadFile(u.path)
	if err != nil {
		// No file yet → empty universe, pipeline idle. Not an error worth failing on.
		u.mu.Lock()
		u.cur = DailyUniverse{}
		u.syms = map[string]UniverseEntry{}
		u.mu.Unlock()
		return nil
	}
	var du DailyUniverse
	if err := json.Unmarshal(b, &du); err != nil {
		return err
	}
	m := make(map[string]UniverseEntry, len(du.Universe))
	for _, e := range du.Universe {
		m[strings.ToUpper(strings.TrimSpace(e.Symbol))] = e
	}
	u.mu.Lock()
	u.cur = du
	u.syms = m
	u.mu.Unlock()
	return nil
}

// Symbols returns today's curated symbol list.
func (u *Universe) Symbols() []string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := make([]string, 0, len(u.syms))
	for s := range u.syms {
		out = append(out, s)
	}
	return out
}

// Has reports whether a symbol is in today's curated universe (i.e. the agents may act on it).
func (u *Universe) Has(sym string) bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	_, ok := u.syms[strings.ToUpper(strings.TrimSpace(sym))]
	return ok
}

// Entry returns the universe metadata for a symbol (catalyst, tier, sentiment lean, flags).
func (u *Universe) Entry(sym string) (UniverseEntry, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	e, ok := u.syms[strings.ToUpper(strings.TrimSpace(sym))]
	return e, ok
}

// Allocation returns today's shared-budget parameters (with safe fallbacks).
func (u *Universe) Allocation() Allocation {
	u.mu.RLock()
	defer u.mu.RUnlock()
	a := u.cur.Allocation
	if a.PerPositionUSD <= 0 {
		a.PerPositionUSD = 2000
	}
	if a.MaxConcurrent <= 0 {
		a.MaxConcurrent = 3
	}
	if a.BudgetUSD <= 0 {
		// Match the quant paper account's actual cash ($8k) rather than per×max (which
		// would imply margin). A daily_universe.json can override budget_usd explicitly.
		a.BudgetUSD = 8000
	}
	return a
}

// Regime returns today's market posture.
func (u *Universe) Regime() MarketRegime {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.cur.MarketRegime
}

// StandDown reports whether today's posture forbids new entries.
func (u *Universe) StandDown() bool {
	return strings.EqualFold(u.Regime().Posture, "stand_down")
}
