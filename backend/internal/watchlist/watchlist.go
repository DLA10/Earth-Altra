// Package watchlist parses the EVENT_DRIVEN_WATCHLIST.md markdown into departments
// (the ## sections that contain a Ticker/Company/Type/Event-Sensitivity table) with
// their tickers and per-ticker catalyst strings. Departments and tickers are parsed
// at load time — never hardcoded.
package watchlist

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// Ticker is one row from a department table.
type Ticker struct {
	Symbol   string `json:"symbol"`
	Company  string `json:"company"`
	Type     string `json:"type"`
	Catalyst string `json:"catalyst"` // the "Event Sensitivity" column
}

// Department is a ## section whose table lists tickers.
type Department struct {
	Name    string   `json:"name"`
	Slug    string   `json:"slug"`
	Icon    string   `json:"icon"` // Tabler icon name for the UI header
	Tickers []Ticker `json:"tickers"`
}

// Watchlist is the parsed file.
type Watchlist struct {
	Departments []Department `json:"departments"`
	Symbols     []string     `json:"symbols"` // unique union across all departments
}

var (
	boldStar = regexp.MustCompile(`\*`)
	dashCell = regexp.MustCompile(`^-+$`)
)

// Load reads and parses the watchlist from the first existing candidate path.
func Load(candidates ...string) (*Watchlist, error) {
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
		// Fall back to the first candidate for a meaningful error.
		path = candidates[0]
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	wl := &Watchlist{}
	seen := map[string]bool{}

	var cur *Department
	var headerSeen bool // whether the current table's header row was the ticker header

	flush := func() {
		if cur != nil && len(cur.Tickers) > 0 {
			wl.Departments = append(wl.Departments, *cur)
		}
		cur = nil
		headerSeen = false
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		// New ## section heading ends the previous department.
		if strings.HasPrefix(line, "## ") {
			flush()
			name := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			cur = &Department{Name: name, Slug: slugify(name), Icon: iconFor(name)}
			continue
		}
		if cur == nil || !strings.HasPrefix(line, "|") {
			continue
		}

		cells := splitRow(line)
		if len(cells) == 0 {
			continue
		}
		// Identify the ticker-table header; ignore other tables (e.g. catalysts).
		if !headerSeen {
			joined := strings.ToLower(strings.Join(cells, "|"))
			if strings.Contains(joined, "ticker") && strings.Contains(joined, "event sensitivity") {
				headerSeen = true
			} else {
				// Some other table under this heading; this isn't a department.
				cur = nil
			}
			continue
		}
		// Skip the |---|---| separator row.
		if dashCell.MatchString(strings.ReplaceAll(cells[0], " ", "")) {
			continue
		}
		if len(cells) < 4 {
			continue
		}
		sym := cleanSymbol(cells[0])
		if sym == "" {
			continue
		}
		cur.Tickers = append(cur.Tickers, Ticker{
			Symbol:   sym,
			Company:  cells[1],
			Type:     cells[2],
			Catalyst: cells[3],
		})
		if !seen[sym] {
			seen[sym] = true
			wl.Symbols = append(wl.Symbols, sym)
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return wl, nil
}

// splitRow splits a markdown table row on '|' and trims each cell.
func splitRow(line string) []string {
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func cleanSymbol(s string) string {
	s = boldStar.ReplaceAllString(s, "")
	return strings.ToUpper(strings.TrimSpace(s))
}

func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// iconFor maps a department name to a Tabler icon (best-effort by keyword).
func iconFor(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "defense"), strings.Contains(n, "military"):
		return "shield-bolt"
	case strings.Contains(n, "energy"), strings.Contains(n, "oil"):
		return "flame"
	case strings.Contains(n, "aviation"), strings.Contains(n, "aerospace"):
		return "plane"
	case strings.Contains(n, "space"), strings.Contains(n, "satellite"):
		return "rocket"
	case strings.Contains(n, "semiconductor"):
		return "cpu"
	case strings.Contains(n, "cloud"), strings.Contains(n, "software"), strings.Contains(n, "data center"):
		return "server"
	case strings.Contains(n, "cyber"):
		return "lock"
	case strings.Contains(n, "auto"), strings.Contains(n, "ev"):
		return "car"
	case strings.Contains(n, "lithium"), strings.Contains(n, "metals"), strings.Contains(n, "materials"):
		return "diamond"
	case strings.Contains(n, "steel"), strings.Contains(n, "chemical"):
		return "building-factory"
	case strings.Contains(n, "agriculture"), strings.Contains(n, "food"):
		return "plant"
	case strings.Contains(n, "shipping"), strings.Contains(n, "logistics"), strings.Contains(n, "rail"):
		return "ship"
	case strings.Contains(n, "power"), strings.Contains(n, "grid"), strings.Contains(n, "utilities"):
		return "bolt"
	case strings.Contains(n, "nuclear"):
		return "atom"
	case strings.Contains(n, "clean energy"), strings.Contains(n, "solar"):
		return "sun"
	case strings.Contains(n, "bank"), strings.Contains(n, "broker"), strings.Contains(n, "credit"):
		return "building-bank"
	case strings.Contains(n, "insurance"):
		return "umbrella"
	case strings.Contains(n, "health"), strings.Contains(n, "pharma"), strings.Contains(n, "biotech"):
		return "heartbeat"
	case strings.Contains(n, "consumer"), strings.Contains(n, "retail"), strings.Contains(n, "restaurant"):
		return "shopping-cart"
	case strings.Contains(n, "housing"), strings.Contains(n, "homebuilder"), strings.Contains(n, "construction"):
		return "home"
	case strings.Contains(n, "industrial"), strings.Contains(n, "machinery"):
		return "settings"
	case strings.Contains(n, "real estate"), strings.Contains(n, "reit"):
		return "buildings"
	case strings.Contains(n, "media"), strings.Contains(n, "telecom"), strings.Contains(n, "gaming"):
		return "device-tv"
	case strings.Contains(n, "crypto"), strings.Contains(n, "bitcoin"):
		return "currency-bitcoin"
	case strings.Contains(n, "quantum"):
		return "atom-2"
	default:
		return "chart-candle"
	}
}
