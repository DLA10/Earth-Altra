// Package config loads runtime configuration from environment variables (and an
// optional .env file). Secrets (Alpaca keys) live ONLY on the server and are never
// surfaced to the browser.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime settings for the server.
type Config struct {
	// Alpaca credentials.
	APIKeyID  string
	APISecret string

	// Live vs paper. When Paper is true we hit Alpaca's paper endpoints.
	Paper bool

	// Endpoints (derived from Paper unless explicitly overridden).
	TradingBaseURL string // e.g. https://api.alpaca.markets
	DataBaseURL    string // e.g. https://data.alpaca.markets
	DataFeed       string // "sip" (Algo Trader Plus) or "iex" (free)

	// Symbols to stream and trade.
	Symbols []string

	// Server.
	HTTPAddr       string // e.g. :8080
	AllowedOrigins []string

	// Safety: reject any single order whose notional value exceeds this cap.
	// 0 disables the cap (NOT recommended for live trading).
	MaxOrderNotional float64

	// DECEPTICON scanner.
	DecepticonEnabled   bool
	WatchlistPath       string // resolved path to EVENT_DRIVEN_WATCHLIST.md
	WatchlistCandidates []string

	// News enrichment for the DECEPTICON movers "why is it moving" feature. All optional:
	// when a key is empty that source is simply skipped (Alpaca news always works).
	GeminiAPIKey   string // on-click article summaries
	GeminiModel    string // default gemini-3.5-flash
	GeminiRPM      int    // self-imposed requests/minute ceiling (stay under the free limit)
	GeminiDailyCap int    // self-imposed requests/day ceiling

	// Dip Watcher Telegram bot: alerts when watched stocks dip and bounce. All optional;
	// disabled unless both the token and chat ID are set.
	TelegramBotToken string
	TelegramChatID   string

	// Quant paper account (a SEPARATE Alpaca paper account, isolated from the live keys
	// above). The AI quant pipeline places its orders here. Empty key = shadow mode.
	PaperClaudeKey    string
	PaperClaudeSecret string
	PaperRbtKey       string
	PaperRbtSecret    string
	// Per-desk paper accounts. STRICT one account per desk: sharing an account lets the
	// desks liquidate each other's shares and corrupts position tracking (the
	// 2026-07-13/14 incident: quant's Rehydrate adopted RIDP's positions and Agent 3
	// sold them). Empty keys = that desk stays OFF — there is no fallback to another
	// desk's account.
	PaperRidpKey    string
	PaperRidpSecret string
	PaperSndkKey    string
	PaperSndkSecret string
	// Breadcrumbs desk: the generalized volatility scalper (SNDK pipeline extended to the
	// validated 22-name volatile basket) with a hard budget tracker and leak-proof book.
	// Its OWN paper account (PAPER_BREADCRUMBS_*). Empty keys = desk OFF.
	PaperBreadcrumbsKey    string
	PaperBreadcrumbsSecret string
	BreadcrumbsLive        bool     // place paper orders (false = shadow log-only)
	BreadcrumbsUniverse    []string // the volatile basket (BC_UNIVERSE)
	BreadcrumbsBudget      float64  // hard cap on total deployed notional (USD)
	BreadcrumbsNotional    float64  // per-trade slice (USD)
	BreadcrumbsMaxSlots    int      // max concurrent positions (0 = one per symbol)
	BreadcrumbsTPPct       float64  // target % (arms trail) — must match the model labels
	BreadcrumbsSLPct       float64  // hard stop %
	BreadcrumbsTrailPct    float64  // trailing width %
	BreadcrumbsLock        bool     // profit-lock: floor the trail at the target
	BreadcrumbsRetrain     bool     // auto-retrain the pooled model monthly (rolling) + boot catch-up
	BreadcrumbsLossCap     float64  // halt NEW entries once the day's realized P&L ≤ -cap (USD, 0 = disabled)
	// Dip+rise desk (Agent 2 dip entries + the rise watcher — one strategy family, both
	// fed by the Telegram dip watcher). Runs on its OWN paper account, separate from the
	// signal pipeline's PAPER_CLAUDE account. Empty keys = the family stays shadow.
	PaperDipKey    string
	PaperDipSecret string
	// SURGER v2 lab: three validated continuation detectors trading live paper on the
	// dip+rise account with srg*_ coid attribution (see SURGER_V2.md).
	SurgerLive     bool    // run the SURGER lab (needs PAPER_DIP keys)
	SurgerNotional float64 // per-trade slice USD
	SurgerSlots    int     // max concurrent positions PER VARIANT

	// Anthropic key for the quant agents (entry/exit/review). Empty = agents stay idle.
	AnthropicAPIKey string
	// ClaudeSymbols are always-streamed symbols for the quant pipeline (besides SPY/QQQ).
	ClaudeSymbols []string

	// Quant pipeline (dip-driven multi-agent). Agent 4 sentiment runs on a local Ollama model.
	OllamaEndpoint string
	OllamaModel    string
	// QuantSentiment gates Agent 4 (the local-Ollama sentiment enrichment). It is ADVISORY
	// only — its score merely tweaks Agent 2's snapshot and allocator ranking, and the whole
	// dip/rise desk runs fine without it (as it already does when Ollama is offline). false =
	// skip wiring Agent 4 entirely (no goroutine, no failed-request log spam). Default true.
	QuantSentiment   bool
	QuantEntryModel  string  // Agent 2 (entry) model
	QuantExitModel   string  // Agent 3 (exit) model
	QuantReviewModel string  // daily review model
	QuantTrailPct    float64 // deterministic trailing-stop floor %
	QuantLive        bool    // false = shadow (decide/log/label only); true = place paper orders
	// QuantOvernightCap allows holding at most this much position VALUE (USD) past the
	// close — the single best-performing winner only; everything else still flattens at
	// 15:55 ET. 0 (default) = flatten everything, no overnight risk.
	QuantOvernightCap float64

	// Quant signal engine: the curated ~100-symbol universe file (QUANT_UNIVERSE.json).
	QuantUniverseCandidates []string
	// QuantSignalsLive routes signal-engine entries (all strategies + the learned
	// time-of-day gate — the validated Tier-1 config) to the PAPER broker via the LLM
	// judge + allocator + manager. false = shadow journaling only.
	QuantSignalsLive bool
	// QuantJudgeModel is the signal entry judge's model.
	QuantJudgeModel string
	// QuantDailyLossCap halts new signal entries once the day's approximate realized
	// P&L reaches -cap (USD).
	QuantDailyLossCap float64
	// QuantClfGate enables the promoted ML entry gate (RESEARCH_BACKLOG #15): nightly-
	// trained per-strategy LightGBM classifiers score every signal's expected R and the
	// trader rejects entries below the pre-registered 0.03 margin. Fail-open: without
	// fresh models the desk trades exactly as before. Paper-side only.
	QuantClfGate bool
	// QuantRetrain auto-runs ml/train_live.py each weekday ~17:05 ET (plus a boot
	// catch-up when models are stale) so the gate walks forward with the live journal.
	QuantRetrain bool
	// QuantTODGate: when true the signal trader ENFORCES the learned time-of-day gate
	// (skips entries in buckets with proven negative expectancy). Default FALSE
	// (shadow-only) since the 2026-07 re-validation: cumulative buckets fail across a
	// regime change (12-mo: −$718 base → −$961 gated) and decayed buckets are too
	// data-starved to beat no-gate on any window tested. The engine still journals
	// tod_bucket/tod_blocked per signal and keeps decayed stats fresh for a future
	// re-review; flipping this to true re-enforces without a code change.
	QuantTODGate bool
	// QuantAlignGate enforces the trend-alignment playbook (signals/alignment.go): each
	// strategy trades only its proven (market trend, stock trend) cells from the
	// 12-month regime study. Deterministic; unknown trends fail open. Default true.
	QuantAlignGate bool
	// RidpLive routes the RIDP two-strategy desk's orders (RIDER + DIPPER, both fully
	// deterministic) to the paper-claude account. false = shadow (journals only).
	RidpLive bool
	// QuantRiseLive routes rising-watcher entries to the paper broker: dips Agent 2
	// declined that then print a CONFIRMED rise (validated on the 2026-07-06..08 replay:
	// +0.37R mean vs a negative edge buying at detection). Default FALSE = shadow: the
	// watcher arms, journals, and alerts every trigger but places no orders.
	QuantRiseLive bool
	// QuantStrategistModel is the pre-market Strategist agent's model ("" uses default).
	QuantStrategistModel string
	// QuantStrategist enables the pre-market posture/allocation agent.
	QuantStrategist bool
	// ResearchLoop auto-runs ml/research_loop.py at 13:30 ET (market open + 4h) on
	// weekdays and delivers the report to Telegram. Proposals are never auto-applied.
	ResearchLoop bool
}

const (
	liveTradingURL  = "https://api.alpaca.markets"
	paperTradingURL = "https://paper-api.alpaca.markets"
	dataURL         = "https://data.alpaca.markets"
)

// Load reads configuration from the environment. It looks for a .env file in the
// working directory and the backend directory but does not require one.
func Load() (*Config, error) {
	// Best-effort .env load; ignore "not found".
	_ = godotenv.Load(".env", "../.env", "backend/.env")

	c := &Config{
		APIKeyID:          strings.TrimSpace(os.Getenv("APCA_API_KEY_ID")),
		APISecret:         strings.TrimSpace(os.Getenv("APCA_API_SECRET_KEY")),
		Paper:             envBool("ALPACA_PAPER", false),
		DataFeed:          envStr("ALPACA_DATA_FEED", "sip"),
		HTTPAddr:          envStr("HTTP_ADDR", ":8080"),
		MaxOrderNotional:  envFloat("MAX_ORDER_NOTIONAL", 25000),
		DecepticonEnabled: envBool("DECEPTICON_ENABLED", true),

		GeminiAPIKey:   strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiModel:    envStr("GEMINI_MODEL", "gemini-3.5-flash"),
		GeminiRPM:      int(envFloat("GEMINI_RPM", 8)),
		GeminiDailyCap: int(envFloat("GEMINI_DAILY_CAP", 200)),

		TelegramBotToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChatID:   strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),

		PaperClaudeKey:    strings.TrimSpace(os.Getenv("PAPER_CLAUDE_KEY")),
		PaperClaudeSecret: strings.TrimSpace(os.Getenv("PAPER_CLAUDE_SECRET")),
		PaperRbtKey:       strings.TrimSpace(os.Getenv("PAPER_RBT_KEY")),
		PaperRbtSecret:    strings.TrimSpace(os.Getenv("PAPER_RBT_SECRET")),
		PaperRidpKey:      strings.TrimSpace(os.Getenv("PAPER_RIDP_KEY")),
		PaperRidpSecret:   strings.TrimSpace(os.Getenv("PAPER_RIDP_SECRET")),
		PaperSndkKey:      strings.TrimSpace(os.Getenv("PAPER_SNDK_KEY")),
		PaperSndkSecret:   strings.TrimSpace(os.Getenv("PAPER_SNDK_SECRET")),

		PaperBreadcrumbsKey:    strings.TrimSpace(os.Getenv("PAPER_BREADCRUMBS_KEY")),
		PaperBreadcrumbsSecret: strings.TrimSpace(os.Getenv("PAPER_BREADCRUMBS_SECRET")),
		BreadcrumbsLive:        envBool("BC_LIVE", true),
		BreadcrumbsBudget:      envFloat("BC_BUDGET", 200000),
		BreadcrumbsNotional:    envFloat("BC_NOTIONAL", 2000),
		BreadcrumbsMaxSlots:    int(envFloat("BC_MAX_SLOTS", 0)),
		BreadcrumbsTPPct:       envFloat("BC_TP_PCT", 0.0057),
		BreadcrumbsSLPct:       envFloat("BC_SL_PCT", 0.0071),
		BreadcrumbsTrailPct:    envFloat("BC_TRAIL_PCT", 0.002),
		BreadcrumbsLock:        envBool("BC_LOCK", true),
		BreadcrumbsRetrain:     envBool("BC_RETRAIN", true),
		BreadcrumbsLossCap:     envFloat("BC_DAILY_LOSS_CAP", 500),
		PaperDipKey:            strings.TrimSpace(os.Getenv("PAPER_DIP_KEY")),
		PaperDipSecret:         strings.TrimSpace(os.Getenv("PAPER_DIP_SECRET")),
		SurgerLive:             envBool("SURGER_LIVE", true),
		SurgerNotional:         envFloat("SURGER_NOTIONAL", 5000),
		SurgerSlots:            int(envFloat("SURGER_SLOTS", 5)),

		AnthropicAPIKey:      strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		ClaudeSymbols:        splitCSV(envStr("CLAUDE_SYMBOLS", "SNDK,MU")),
		OllamaEndpoint:       envStr("OLLAMA_ENDPOINT", "http://localhost:11434"),
		OllamaModel:          envStr("OLLAMA_MODEL", "gemma2:2b"),
		QuantSentiment:       envBool("QUANT_SENTIMENT", true),
		QuantEntryModel:      envStr("QUANT_ENTRY_MODEL", "claude-haiku-4-5"),
		QuantExitModel:       envStr("QUANT_EXIT_MODEL", "claude-haiku-4-5"),
		QuantReviewModel:     envStr("QUANT_REVIEW_MODEL", "claude-opus-4-8"),
		QuantTrailPct:        envFloat("QUANT_TRAIL_PCT", 1.5),
		QuantLive:            envBool("QUANT_LIVE", true),
		QuantOvernightCap:    envFloat("QUANT_OVERNIGHT_CAP", 0),
		QuantSignalsLive:     envBool("QUANT_SIGNALS_LIVE", true),
		QuantJudgeModel:      envStr("QUANT_JUDGE_MODEL", "claude-haiku-4-5"),
		QuantDailyLossCap:    envFloat("QUANT_DAILY_LOSS_CAP", 150),
		QuantClfGate:         envBool("QUANT_CLF_GATE", true),
		QuantRetrain:         envBool("QUANT_RETRAIN", true),
		QuantTODGate:         envBool("QUANT_TOD_GATE", false),
		QuantRiseLive:        envBool("QUANT_RISE_LIVE", false),
		RidpLive:             envBool("RIDP_LIVE", true),
		QuantAlignGate:       envBool("QUANT_ALIGN_GATE", true),
		QuantStrategistModel: envStr("QUANT_STRATEGIST_MODEL", "claude-opus-4-8"),
		QuantStrategist:      envBool("QUANT_STRATEGIST", true),
		ResearchLoop:         envBool("RESEARCH_LOOP", true),
	}

	c.QuantUniverseCandidates = []string{
		envStr("QUANT_UNIVERSE_PATH", ""),
		"../QUANT_UNIVERSE.json",
		"QUANT_UNIVERSE.json",
	}

	// Watchlist path: explicit override, else common locations relative to the
	// backend working directory.
	c.WatchlistCandidates = []string{
		envStr("WATCHLIST_PATH", ""),
		"../EVENT_DRIVEN_WATCHLIST.md",
		"EVENT_DRIVEN_WATCHLIST.md",
		"backend/EVENT_DRIVEN_WATCHLIST.md",
	}

	c.Symbols = splitCSV(envStr("SYMBOLS", "SNDK,SPCX,STX,NVDA,MRVL"))
	// The validated 22-name high-volatility basket (kept in sync with ml/train_breadcrumbs_model.py).
	c.BreadcrumbsUniverse = splitCSV(envStr("BC_UNIVERSE",
		"NVDA,AMD,MU,SMCI,MRVL,ARM,PLTR,COIN,TSLA,MSTR,IONQ,CRWV,WDC,ON,LRCX,DELL,ANET,SNOW,HOOD,RGTI,ASTS,RIOT"))
	c.AllowedOrigins = splitCSV(envStr("ALLOWED_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173"))

	// Derive endpoints from the paper flag unless explicitly overridden.
	if c.Paper {
		c.TradingBaseURL = envStr("ALPACA_TRADING_URL", paperTradingURL)
	} else {
		c.TradingBaseURL = envStr("ALPACA_TRADING_URL", liveTradingURL)
	}
	c.DataBaseURL = envStr("ALPACA_DATA_URL", dataURL)

	if c.APIKeyID == "" || c.APISecret == "" {
		return c, fmt.Errorf("missing Alpaca credentials: set APCA_API_KEY_ID and APCA_API_SECRET_KEY (see .env.example)")
	}
	return c, nil
}

// Mode returns a human-readable "LIVE" or "PAPER".
func (c *Config) Mode() string {
	if c.Paper {
		return "PAPER"
	}
	return "LIVE"
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, strings.ToUpper(t))
		}
	}
	return out
}
