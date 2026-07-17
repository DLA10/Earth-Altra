package quant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Strategist is the pre-market agent: each trading morning it reads the deterministic
// market state (SPY/QQQ trend, volatility, 20-day-MA posture), the eval scoreboard, and
// the latest daily review, and writes the day's posture + allocation to
// data/daily_universe.json — the same contract the manual Instruction.md ritual used, so
// everything downstream (allocator reload, stand_down gates, cautious conviction bar)
// just works. The LLM chooses posture/notes within DETERMINISTIC clamps (budget,
// per-position, and concurrency are bounded in code); an API failure falls back to a
// pure-rules posture so the desk never starts the day unconfigured.
//
// Honesty note: the Strategist has no web access, so it cannot see the economic
// calendar; its playbook says so and biases toward caution when volatility says event
// risk. The LangGraph research loop / the human can override the file at any time.
type Strategist struct {
	client   *Anthropic
	model    string
	dataDir  string
	loc      *time.Location
	dlog     *DecisionLog
	marketFn func() (MarketState, error) // deterministic pre-market inputs
	reload   func()                      // e.g. universe.Reload + allocator reconfigure
	symbols  []string                    // tradable symbols from the master watchlist
}

// MarketState is the deterministic pre-market picture handed to the model.
type MarketState struct {
	SpyPct5d     float64 `json:"spy_pct_5d"`
	QqqPct5d     float64 `json:"qqq_pct_5d"`
	QqqAbove20d  bool    `json:"qqq_above_20d_ma"`
	SpyAbove20d  bool    `json:"spy_above_20d_ma"`
	QqqATRPct    float64 `json:"qqq_atr_pct"` // 14d ATR as % of price (vol proxy)
	PrevDayQqq   float64 `json:"qqq_prev_day_pct"`
}

func NewStrategist(client *Anthropic, model, dataDir string, loc *time.Location, dlog *DecisionLog, marketFn func() (MarketState, error), reload func(), symbols []string) *Strategist {
	if strings.TrimSpace(model) == "" {
		model = "claude-opus-4-8"
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Strategist{
		client:   client,
		model:    model,
		dataDir:  dataDir,
		loc:      loc,
		dlog:     dlog,
		marketFn: marketFn,
		reload:   reload,
		symbols:  symbols,
	}
}

// loadUniverseSymbols reads and parses the first available master watchlist json file.
func loadUniverseSymbols(candidates ...string) []string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err != nil {
			continue
		}
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		var u struct {
			Sectors map[string][]string `json:"sectors"`
		}
		if err := json.Unmarshal(b, &u); err != nil {
			continue
		}
		var syms []string
		for _, sList := range u.Sectors {
			for _, sym := range sList {
				s := strings.ToUpper(strings.TrimSpace(sym))
				if s != "" {
					syms = append(syms, s)
				}
			}
		}
		if len(syms) > 0 {
			sort.Strings(syms)
			return syms
		}
	}
	return nil
}

var strategistSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"posture":          map[string]interface{}{"type": "string", "enum": []string{"normal", "cautious", "stand_down"}},
		"budget_usd":       map[string]interface{}{"type": "number", "description": "shared budget for the day (≤ 8000)"},
		"per_position_usd": map[string]interface{}{"type": "number", "description": "full-slice position size (≤ 2500)"},
		"max_concurrent":   map[string]interface{}{"type": "integer", "description": "max open positions (1..3)"},
		"notes":            map[string]interface{}{"type": "string", "description": "one-line rationale"},
	},
	"required":             []string{"posture", "budget_usd", "per_position_usd", "max_concurrent", "notes"},
	"additionalProperties": false,
}

// RunDaily generates the day's config once each weekday in the 08:50–09:25 ET window.
// Boot catch-up: if the process starts LATER in the trading day and today's config was
// never written (e.g. the backend was down pre-market), it generates immediately —
// otherwise yesterday's posture/allocation would silently govern today.
func (s *Strategist) RunDaily(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	lastDay := ""
	if now := time.Now().In(s.loc); now.Weekday() >= time.Monday && now.Weekday() <= time.Friday &&
		now.Hour() >= 8 && now.Hour() < 15 && !s.freshFor(now.Format("2006-01-02")) {
		day := now.Format("2006-01-02")
		if err := s.Generate(day); err != nil {
			log.Printf("[strategist] boot catch-up for %s failed: %v", day, err)
		} else {
			lastDay = day
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().In(s.loc)
			day := now.Format("2006-01-02")
			weekday := now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
			window := (now.Hour() == 8 && now.Minute() >= 50) || (now.Hour() == 9 && now.Minute() <= 25)
			if weekday && window && day != lastDay {
				if err := s.Generate(day); err != nil {
					log.Printf("[strategist] %s failed: %v (will retry this window)", day, err)
					continue
				}
				lastDay = day
			}
		}
	}
}

// Generate produces and writes the day's config (LLM within clamps; rules fallback).
func (s *Strategist) Generate(day string) error {
	ms, err := s.marketFn()
	if err != nil {
		return fmt.Errorf("market state: %w", err)
	}
	digest := map[string]interface{}{"day": day, "market": ms}
	if b, e := os.ReadFile(filepath.Join(s.dataDir, "evals", "scoreboard.json")); e == nil {
		digest["scoreboard"] = json.RawMessage(b)
	}
	if rv := latestFile(filepath.Join(s.dataDir, "reviews")); rv != "" {
		if b, e := os.ReadFile(rv); e == nil {
			digest["latest_review"] = json.RawMessage(b)
		}
	}
	in, _ := json.Marshal(digest)

	posture, budget, perPos, maxConc, notes := s.fallback(ms)
	if s.client.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		raw, usage, err := s.client.Call(ctx, s.model, Constitution+"\n\n"+strategistPlaybook,
			"record_daily_config", "Record today's market posture and allocation.", strategistSchema, string(in), 800)
		cancel()
		if err != nil {
			log.Printf("[strategist] LLM failed (%v) — using rules fallback", err)
		} else {
			var out struct {
				Posture        string  `json:"posture"`
				BudgetUSD      float64 `json:"budget_usd"`
				PerPositionUSD float64 `json:"per_position_usd"`
				MaxConcurrent  int     `json:"max_concurrent"`
				Notes          string  `json:"notes"`
			}
			if json.Unmarshal(raw, &out) == nil {
				posture, budget, perPos, maxConc, notes = out.Posture, out.BudgetUSD, out.PerPositionUSD, out.MaxConcurrent, out.Notes
			}
			s.dlog.Append(LogRecord{Agent: "strategist", Event: "decision", Model: s.model,
				Input: json.RawMessage(in), Output: json.RawMessage(raw), Tokens: &usage})
		}
	}

	// Deterministic clamps — the model proposes, code disposes.
	switch posture {
	case "normal", "cautious", "stand_down":
	default:
		posture = "cautious"
	}
	budget = clamp(budget, 1000, 8000)
	perPos = clamp(perPos, 500, 2500)
	if maxConc < 1 {
		maxConc = 1
	}
	if maxConc > 3 {
		maxConc = 3
	}

	symbols := s.symbols
	if len(symbols) == 0 {
		symbols = loadUniverseSymbols("../QUANT_UNIVERSE.json", "QUANT_UNIVERSE.json", filepath.Join(s.dataDir, "../QUANT_UNIVERSE.json"))
	}

	var universeEntries []UniverseEntry
	for _, sym := range symbols {
		sUpper := strings.ToUpper(strings.TrimSpace(sym))
		if sUpper == "" || sUpper == "SPY" || sUpper == "QQQ" || sUpper == "SMH" {
			continue
		}
		universeEntries = append(universeEntries, UniverseEntry{
			Symbol:        sUpper,
			Tier:          1,
			Catalyst:      "Sector trend candidate",
			SentimentLean: "neutral",
			RiskFlags:     []string{},
			Notes:         "automatically copied from master list",
		})
	}
	if len(universeEntries) == 0 {
		universeEntries = []UniverseEntry{}
	}

	du := DailyUniverse{
		Date:        day,
		GeneratedAt: time.Now().In(s.loc).Format("15:04"),
		MarketRegime: MarketRegime{
			Posture: posture,
			SpyBias: bias(ms.SpyPct5d), QqqBias: bias(ms.QqqPct5d),
			Notes: notes,
		},
		Allocation: Allocation{BudgetUSD: budget, PerPositionUSD: perPos, MaxConcurrent: maxConc,
			Notes: "strategist-generated; clamped in code"},
		Universe: universeEntries,
		Excluded: []Excluded{},
	}
	b, err := json.MarshalIndent(du, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "daily_universe.json"), b, 0o644); err != nil {
		return err
	}
	// Archive a dated copy so the morning-config history survives (the live file above
	// is overwritten daily; the archive never is).
	if dir := filepath.Join(s.dataDir, "strategist"); os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(filepath.Join(dir, day+".json"), b, 0o644)
	}
	if s.reload != nil {
		s.reload()
	}
	log.Printf("[strategist] %s: posture=%s budget=$%.0f per=$%.0f conc=%d — %s", day, posture, budget, perPos, maxConc, notes)
	return nil
}

// freshFor reports whether daily_universe.json already carries today's date.
func (s *Strategist) freshFor(day string) bool {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "daily_universe.json"))
	if err != nil {
		return false
	}
	var du struct {
		Date string `json:"date"`
	}
	return json.Unmarshal(b, &du) == nil && du.Date == day
}

// fallback is the pure-rules config used when the LLM is unavailable. Below-MA alone is
// NOT cautious anymore (the 12-month regime study showed the desk earns on below-MA days
// and the alignment gate picks the right strategy families per regime); caution is about
// disorder — elevated vol or a sharp red day — and stand-down is for crash tapes.
func (s *Strategist) fallback(ms MarketState) (string, float64, float64, int, string) {
	posture := "normal"
	if ms.QqqATRPct > 3 || ms.PrevDayQqq < -2.5 {
		posture = "cautious"
	}
	if ms.QqqPct5d < -5 && ms.QqqATRPct > 3.5 {
		posture = "stand_down"
	}
	return posture, 8000, 2000, 3, "rules fallback (LLM unavailable): posture from volatility/crash state (alignment gate handles regime)"
}

func bias(pct float64) string {
	switch {
	case pct > 0.5:
		return "up"
	case pct < -0.5:
		return "down"
	default:
		return "flat"
	}
}

func latestFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	latest := ""
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".json") && n > latest {
			latest = n
		}
	}
	if latest == "" {
		return ""
	}
	return filepath.Join(dir, latest)
}

const strategistPlaybook = `YOUR ROLE: You are the pre-market STRATEGIST for an intraday long-only quant desk trading a
curated large-cap tech universe on a PAPER account. Each morning you receive a deterministic
market digest (SPY/QQQ 5-day trend, 20-day-MA state, volatility proxy, yesterday's move), the
strategy scoreboard (rolling counterfactual expectancy per strategy, demotions), and the latest
post-market review. Set today's POSTURE and ALLOCATION.

You do NOT have the economic calendar or news access. If volatility is elevated (qqq_atr_pct
high) or the tape just broke trend, assume event risk exists and lean cautious.

POSTURE (gates how aggressively the desk trades — hard rules downstream):
- "normal": orderly vol, whether QQQ is above OR below its 20d MA. IMPORTANT — a below-MA
  tape is NOT by itself a reason to cut: the desk's 12-month regime study (22k outcomes)
  shows its strategies EARN most on below-MA days (+144R) and bleed on above-MA days
  (−624R), and a deterministic trend-alignment gate downstream already selects which
  strategy families may trade each regime. Trust it; keep normal allocation.
- "cautious": DISORDERLY conditions — elevated/expanding vol (qqq_atr_pct high), a sharp
  falling-knife day yesterday, or a heavy losing streak on the scoreboard. Downstream,
  entries then require higher conviction and you should trim per_position_usd and/or
  max_concurrent. Caution is about chaos, not direction.
- "stand_down": crash conditions — QQQ down very sharply over 5 days AND vol extreme.
  No new entries all day.

ALLOCATION (clamped in code regardless: budget ≤ $8000, per-position ≤ $2500, concurrent ≤ 3):
- normal day: budget 8000, per_position 2000, max_concurrent 3.
- cautious: cut per_position to ~1000-1500 and/or max_concurrent to 2.
- stand_down: allocation is moot (entries are blocked) — set conservative values anyway.

Weigh the scoreboard: if MOST strategy families are bleeding (negative mean_r, demotions),
that is evidence the tape is hostile to the desk as a whole — lean cautious even on a green
tape. A single bleeding family is the alignment gate's and scoreboard's job, not a posture
reason.

notes: ONE plain-English line a novice can read explaining today's call. No hedging lists.
Call record_daily_config.`
