package quant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TokenUsage records an LLM call's token accounting (for per-agent cost attribution).
type TokenUsage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cache_read,omitempty"`
	CacheCreate int `json:"cache_create,omitempty"`
}

// LogRecord is one structured line in the decision log. Everything the agents do — every
// decision, the input that drove it, the resulting order, and the eventual outcome — is recorded
// so the daily review can measure CONSISTENCY (and so we can prove whether changes help).
type LogRecord struct {
	Time   time.Time   `json:"time"`
	Day    string      `json:"day"`   // ET date, for grouping
	Agent  string      `json:"agent"` // agent2_entry | agent3_exit | agent4_sentiment | allocator | review | pipeline
	Event  string      `json:"event"` // decision | order | outcome | skip | dip | error
	Symbol string      `json:"symbol,omitempty"`
	Model  string      `json:"model,omitempty"`
	Input  interface{} `json:"input,omitempty"`
	Output interface{} `json:"output,omitempty"`
	Tokens *TokenUsage `json:"tokens,omitempty"`
	Note   string      `json:"note,omitempty"`
}

// DecisionLog appends structured JSONL to <dataDir>/decisions/YYYY-MM-DD.jsonl (one file per ET
// day). Thread-safe; best-effort (a logging failure never blocks trading).
type DecisionLog struct {
	dir string
	loc *time.Location
	mu  sync.Mutex
}

func NewDecisionLog(dataDir string, loc *time.Location) *DecisionLog {
	if loc == nil {
		loc = time.UTC
	}
	return &DecisionLog{dir: filepath.Join(dataDir, "decisions"), loc: loc}
}

// Append writes one record (stamping Time/Day if unset). Errors are swallowed by design.
func (l *DecisionLog) Append(r LogRecord) {
	if l == nil {
		return
	}
	now := time.Now().In(l.loc)
	if r.Time.IsZero() {
		r.Time = now
	}
	if r.Day == "" {
		r.Day = now.Format("2006-01-02")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, r.Day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// ReadDay returns all records for an ET date (used by the daily review agent). Returns nil if
// there's no log for that day.
func (l *DecisionLog) ReadDay(day string) ([]LogRecord, error) {
	b, err := os.ReadFile(filepath.Join(l.dir, day+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LogRecord
	for _, line := range splitLines(b) {
		if len(line) == 0 {
			continue
		}
		var r LogRecord
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
