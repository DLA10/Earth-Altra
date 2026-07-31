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

	PaperRbtKey       string
	PaperRbtSecret    string
	// Per-desk paper accounts. STRICT one account per desk: sharing an account lets the
	// desks liquidate each other's shares and corrupts position tracking (the
	// 2026-07-13/14 incident: quant's Rehydrate adopted RIDP's positions and Agent 3
	// sold them). Empty keys = that desk stays OFF — there is no fallback to another
	// desk's account.
	PaperRidpKey    string
	PaperRidpSecret string
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
	BreadcrumbsRetrain     bool     // auto-retrain the pooled model on its rolling window + boot catch-up
	BreadcrumbsRetrainDays int      // retrain staleness threshold in days (operator 2026-07-25: weekly)
	BreadcrumbsLossCap     float64  // halt NEW entries once the day's realized P&L ≤ -cap (USD, 0 = disabled)
	BreadcrumbsCutUSD      float64  // live $-cut experiment: flatten any position at -N USD unrealized (0 = off)
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

	// QuantUniverseCandidates points at QUANT_UNIVERSE.json — the curated trading symbol
	// set. NAME IS LEGACY: the AI quant desk it was built for was removed 2026-07-31, but
	// the file is load-bearing for RIDP, SURGER, the regime detector, RBT's baseline and
	// the SIP bar subscription. Do not delete this field.
	QuantUniverseCandidates []string

	// RidpLive routes RIDP desk entries to its paper broker (false = shadow journaling).
	RidpLive bool
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

		PaperRbtKey:       strings.TrimSpace(os.Getenv("PAPER_RBT_KEY")),
		PaperRbtSecret:    strings.TrimSpace(os.Getenv("PAPER_RBT_SECRET")),
		PaperRidpKey:      strings.TrimSpace(os.Getenv("PAPER_RIDP_KEY")),
		PaperRidpSecret:   strings.TrimSpace(os.Getenv("PAPER_RIDP_SECRET")),

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
		BreadcrumbsRetrainDays: int(envFloat("BC_RETRAIN_DAYS", 7)),
		BreadcrumbsLossCap:     envFloat("BC_DAILY_LOSS_CAP", 500),
		BreadcrumbsCutUSD:      envFloat("BC_CUT_USD", 0),
		PaperDipKey:            strings.TrimSpace(os.Getenv("PAPER_DIP_KEY")),
		PaperDipSecret:         strings.TrimSpace(os.Getenv("PAPER_DIP_SECRET")),
		SurgerLive:             envBool("SURGER_LIVE", true),
		SurgerNotional:         envFloat("SURGER_NOTIONAL", 5000),
		SurgerSlots:            int(envFloat("SURGER_SLOTS", 5)),

		RidpLive: envBool("RIDP_LIVE", true),
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
