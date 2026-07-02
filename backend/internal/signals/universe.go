package signals

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Universe is the curated quant trading set (QUANT_UNIVERSE.json): ~100 liquid,
// financially solid names grouped by trend sector, plus context symbols (SPY/QQQ/SMH)
// that are streamed for the market backdrop but never traded.
type Universe struct {
	Updated        string              `json:"updated"`
	Note           string              `json:"note"`
	ContextSymbols []string            `json:"context_symbols"`
	Sectors        map[string][]string `json:"sectors"`

	sectorOf map[string]string
}

// LoadUniverse reads the first existing candidate path.
func LoadUniverse(candidates ...string) (*Universe, error) {
	var path string
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		return nil, fmt.Errorf("no universe file found (tried %v)", candidates)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var u Universe
	if err := json.Unmarshal(b, &u); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	u.sectorOf = map[string]string{}
	for sector, syms := range u.Sectors {
		for _, s := range syms {
			u.sectorOf[strings.ToUpper(strings.TrimSpace(s))] = sector
		}
	}
	if len(u.sectorOf) == 0 {
		return nil, fmt.Errorf("universe %s has no symbols", path)
	}
	return &u, nil
}

// Symbols returns the sorted tradable symbol list (context symbols excluded).
func (u *Universe) Symbols() []string {
	out := make([]string, 0, len(u.sectorOf))
	for s := range u.sectorOf {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Has reports whether a symbol is tradable in this universe.
func (u *Universe) Has(sym string) bool {
	_, ok := u.sectorOf[strings.ToUpper(sym)]
	return ok
}

// Sector returns the symbol's sector ("" if not in the universe).
func (u *Universe) Sector(sym string) string {
	return u.sectorOf[strings.ToUpper(sym)]
}

// Context returns the market-backdrop symbols (never traded).
func (u *Universe) Context() []string {
	return append([]string(nil), u.ContextSymbols...)
}

// All returns tradable + context symbols (for stream subscriptions).
func (u *Universe) All() []string {
	return append(u.Symbols(), u.Context()...)
}
