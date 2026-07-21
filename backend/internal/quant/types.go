// Package quant is the AI quant team: a dip-driven, event-based trading pipeline that runs on
// an isolated paper account. Detection (dipwatch) → Agent 2 (entry, Opus) → deterministic
// Allocator (shared budget) → Agent 3 (exit, Haiku) with a deterministic stop floor, plus
// Agent 4 (sentiment, local Ollama) and a daily post-market review (Opus).
//
// GUIDING PRINCIPLE (shared "constitution" injected into every agent prompt): CONSISTENCY over
// peak profits — high-probability, risk-controlled, repeatable setups; protect capital first;
// a skipped trade beats a low-quality one. The model PROPOSES; deterministic Go DISPOSES — every
// order passes hard rails, and capital allocation is rules-based, never an LLM guess.
//
// Strict isolation: market DATA comes only from the shared candle engine + flow tracker +
// scanner; the paper key only places orders / reads its own history; the live real-money
// key is never used to trade here.
package quant

import "time"

// Constitution is prepended to every agent's system prompt so the whole team optimizes for the
// same goal.
const Constitution = `You are part of an AI quant team whose single goal is CONSISTENCY, not ` +
	`peak profits. Prefer high-probability, risk-controlled, repeatable setups. Protect capital ` +
	`first. Take small steady gains; never swing for home runs or chase. A skipped trade is ` +
	`better than a low-quality one. When the evidence is mixed, the correct answer is usually to ` +
	`pass. You only propose; deterministic code enforces sizing, budget, and hard risk rails.`

// ---- Daily universe (written by the pre-market Claude session per Instruction.md) ----

type MarketRegime struct {
	Posture    string  `json:"posture"` // normal | cautious | stand_down
	SpyBias    string  `json:"spy_bias"`
	QqqBias    string  `json:"qqq_bias"`
	VIX        float64 `json:"vix"`
	MacroToday string  `json:"macro_today"`
	Notes      string  `json:"notes"`
}

type Allocation struct {
	BudgetUSD      float64 `json:"budget_usd"`
	PerPositionUSD float64 `json:"per_position_usd"`
	MaxConcurrent  int     `json:"max_concurrent"`
	Notes          string  `json:"notes"`
}

type UniverseEntry struct {
	Symbol        string   `json:"symbol"`
	Tier          int      `json:"tier"` // 1 = core, 2 = watch
	Catalyst      string   `json:"catalyst"`
	SentimentLean string   `json:"sentiment_lean"` // positive | neutral | negative
	RiskFlags     []string `json:"risk_flags"`
	Notes         string   `json:"notes"`
}

type Excluded struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

type DailyUniverse struct {
	Date         string          `json:"date"`
	GeneratedAt  string          `json:"generated_at_et"`
	MarketRegime MarketRegime    `json:"market_regime"`
	Allocation   Allocation      `json:"allocation"`
	Universe     []UniverseEntry `json:"universe"`
	Excluded     []Excluded      `json:"excluded"`
}

// ---- Dip event: the anatomy of a detected dip, captured at trigger time for Agent 2 ----

type DipEvent struct {
	Symbol       string    `json:"symbol"`
	DetectedAt   time.Time `json:"detected_at"`
	Price        float64   `json:"price"`         // current (bounce-confirmation) price
	PreDipHigh   float64   `json:"pre_dip_high"`  // session high before the pullback
	DipLow       float64   `json:"dip_low"`       // the low of the dip
	DepthPct     float64   `json:"depth_pct"`     // % off the pre-dip high
	DepthATR     float64   `json:"depth_atr"`     // depth in units of daily ATR (how meaningful)
	DurationMin  float64   `json:"duration_min"`  // minutes from pre-dip high to the dip low
	Shape        string    `json:"shape"`         // sharp_v | grinding
	DipVolume    float64   `json:"dip_volume"`    // volume during the drop (capitulation?)
	BounceVolume float64   `json:"bounce_volume"` // volume on the confirming green candle
	RVOL         float64   `json:"rvol"`
	RSI          float64   `json:"rsi"`
	VWAP         float64   `json:"vwap"`
	PriceVsVWAP  float64   `json:"price_vs_vwap_pct"`
}

// ---- Structured agent outputs (produced via forced tool calls; never free-text parsed) ----

// EntryDecision is Agent 2's verdict on a dip.
type EntryDecision struct {
	Action     string  `json:"action"`     // buy | no_buy
	Confidence float64 `json:"confidence"` // 0..1 conviction
	Reason     string  `json:"reason"`     // one plain sentence
}

func (d EntryDecision) IsBuy() bool { return d.Action == "buy" }

// ExitAction is Agent 3's small, safe verb set (Go translates these into real orders; the agent
// never touches raw order types). The deterministic stop floor protects the position regardless.
type ExitAction string

const (
	ExitHold        ExitAction = "hold"
	ExitTightenStop ExitAction = "tighten_stop" // ratchet the protective stop UP only
	ExitTakeProfit  ExitAction = "take_profit"  // sell now into strength
	ExitNow         ExitAction = "exit_now"     // sell now (thesis broken)
)

// ExitDecision is Agent 3's output.
type ExitDecision struct {
	Action     ExitAction `json:"action"`
	StopPrice  float64    `json:"stop_price"` // for tighten_stop
	Confidence float64    `json:"confidence"`
	Reason     string     `json:"reason"`
}

// ---- Reconstructed paper state (for the API / Paper·Claude page; realized-only P&L) ----

type QuantPosition struct {
	Symbol        string    `json:"symbol"`
	Qty           float64   `json:"qty"`
	EntryPrice    float64   `json:"entry_price"`
	EntryTime     time.Time `json:"entry_time"`
	MarkPrice     float64   `json:"mark_price"`
	UnrealizedPNL float64   `json:"unrealized_pnl"`
}

type QuantTrade struct {
	Symbol     string    `json:"symbol"`
	EntryTime  time.Time `json:"entry_time"`
	ExitTime   time.Time `json:"exit_time"`
	EntryPrice float64   `json:"entry_price"`
	ExitPrice  float64   `json:"exit_price"`
	Qty        float64   `json:"qty"`
	PNL        float64   `json:"pnl"`
	ExitReason string    `json:"exit_reason"`
}

type QuantState struct {
	RealizedPNL   float64         `json:"realized_pnl"`
	RealizedToday float64         `json:"realized_today"` // trades exited TODAY (ET) only
	UnrealizedPNL float64         `json:"unrealized_pnl"`
	WinRate       float64         `json:"win_rate"`
	TotalTrades   int             `json:"total_trades"`
	Positions     []QuantPosition `json:"positions"`
	Trades        []QuantTrade    `json:"trades"`
}

// SentimentScore is Agent 4's (local LLM) per-symbol read, cached and consumed by Agent 2.
type SentimentScore struct {
	Symbol    string    `json:"symbol"`
	Lean      string    `json:"lean"`  // positive | neutral | negative
	Score     float64   `json:"score"` // -1..+1
	Catalyst  bool      `json:"has_catalyst"`
	Why       string    `json:"why"`
	UpdatedAt time.Time `json:"updated_at"`
}
