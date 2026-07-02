// Package execsym manages the set of symbols tradable on the Execution page: the
// fixed base symbols from config plus any symbols the user adds at runtime from
// DECEPTICON, minus any the user has hidden. Both the added and hidden sets are
// persisted to disk so the user's chosen list survives restarts.
package execsym

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Manager is a thread-safe symbol set: base + added − hidden.
type Manager struct {
	mu     sync.RWMutex
	base   []string
	added  map[string]bool
	hidden map[string]bool // symbols (incl. base) the user removed
	path   string          // persistence file
}

// persisted is the on-disk shape. Kept backward-compatible with the legacy format
// (a bare JSON array of added symbols).
type persisted struct {
	Added  []string `json:"added"`
	Hidden []string `json:"hidden"`
}

// New creates a Manager with the given base symbols and loads any persisted state.
func New(base []string, path string) *Manager {
	m := &Manager{base: upperAll(base), added: map[string]bool{}, hidden: map[string]bool{}, path: path}
	m.load()
	return m
}

func (m *Manager) load() {
	b, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	baseSet := map[string]bool{}
	for _, s := range m.base {
		baseSet[s] = true
	}
	// New object format first; fall back to the legacy bare array of added symbols.
	var p persisted
	if json.Unmarshal(b, &p) == nil && (len(p.Added) > 0 || len(p.Hidden) > 0) {
		for _, s := range p.Added {
			if s = norm(s); s != "" && !baseSet[s] {
				m.added[s] = true
			}
		}
		for _, s := range p.Hidden {
			if s = norm(s); s != "" {
				m.hidden[s] = true
			}
		}
		return
	}
	var syms []string
	if json.Unmarshal(b, &syms) == nil {
		for _, s := range syms {
			if s = norm(s); s != "" && !baseSet[s] {
				m.added[s] = true
			}
		}
	}
}

func (m *Manager) save() {
	_ = os.MkdirAll(filepath.Dir(m.path), 0o755)
	p := persisted{Added: keys(m.added), Hidden: keys(m.hidden)}
	if b, err := json.MarshalIndent(p, "", "  "); err == nil {
		_ = os.WriteFile(m.path, b, 0o644)
	}
}

// IsBase reports whether sym is a fixed base symbol.
func (m *Manager) IsBase(sym string) bool {
	sym = norm(sym)
	for _, s := range m.base {
		if s == sym {
			return true
		}
	}
	return false
}

// Add records a symbol. If it was hidden, it's un-hidden (re-shown); otherwise it's
// added to the dynamic set. Returns false if it's already visible.
func (m *Manager) Add(sym string) bool {
	sym = norm(sym)
	if sym == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hidden[sym] {
		delete(m.hidden, sym)
		if !m.IsBase(sym) {
			m.added[sym] = true
		}
		m.save()
		return true
	}
	if m.IsBase(sym) || m.added[sym] {
		return false
	}
	m.added[sym] = true
	m.save()
	return true
}

// Remove hides a symbol so it no longer appears in the Execution list. Works for both
// base and added symbols, and the removal is persisted. Returns false if it wasn't
// visible to begin with.
func (m *Manager) Remove(sym string) bool {
	sym = norm(sym)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hidden[sym] {
		return false
	}
	switch {
	case m.added[sym]:
		delete(m.added, sym)
	case m.IsBase(sym):
		m.hidden[sym] = true
	default:
		return false
	}
	m.save()
	return true
}

// Added returns the sorted list of dynamically-added (non-hidden) symbols.
func (m *Manager) Added() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.added))
	for s := range m.added {
		if !m.hidden[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// All returns visible base symbols (in config order) followed by sorted added ones.
func (m *Manager) All() []string {
	m.mu.RLock()
	out := make([]string, 0, len(m.base)+len(m.added))
	for _, s := range m.base {
		if !m.hidden[s] {
			out = append(out, s)
		}
	}
	m.mu.RUnlock()
	return append(out, m.Added()...)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func norm(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func upperAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, norm(s))
	}
	return out
}
