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

	// Anthropic key for the quant agents (entry/exit/review). Empty = agents stay idle.
	AnthropicAPIKey string
	// ClaudeSymbols are always-streamed symbols for the quant pipeline (besides SPY/QQQ).
	ClaudeSymbols []string

	// Quant pipeline (dip-driven multi-agent). Agent 4 sentiment runs on a local Ollama model.
	OllamaEndpoint   string
	OllamaModel      string
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
	// QuantStrategistModel is the pre-market Strategist agent's model ("" uses default).
	QuantStrategistModel string
	// QuantStrategist enables the pre-market posture/allocation agent.
	QuantStrategist bool
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

		AnthropicAPIKey:     strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		ClaudeSymbols:       splitCSV(envStr("CLAUDE_SYMBOLS", "SNDK,MU")),
		OllamaEndpoint:      envStr("OLLAMA_ENDPOINT", "http://localhost:11434"),
		OllamaModel:         envStr("OLLAMA_MODEL", "gemma2:2b"),
		QuantEntryModel:     envStr("QUANT_ENTRY_MODEL", "claude-haiku-4-5"),
		QuantExitModel:      envStr("QUANT_EXIT_MODEL", "claude-haiku-4-5"),
		QuantReviewModel:    envStr("QUANT_REVIEW_MODEL", "claude-opus-4-8"),
		QuantTrailPct:       envFloat("QUANT_TRAIL_PCT", 1.5),
		QuantLive:           envBool("QUANT_LIVE", true),
		QuantOvernightCap:   envFloat("QUANT_OVERNIGHT_CAP", 0),
		QuantSignalsLive:    envBool("QUANT_SIGNALS_LIVE", true),
		QuantJudgeModel:     envStr("QUANT_JUDGE_MODEL", "claude-haiku-4-5"),
		QuantDailyLossCap:   envFloat("QUANT_DAILY_LOSS_CAP", 150),
		QuantStrategistModel: envStr("QUANT_STRATEGIST_MODEL", "claude-opus-4-8"),
		QuantStrategist:      envBool("QUANT_STRATEGIST", true),
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
