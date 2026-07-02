package alpaca

import "time"

// OrderRequest is our internal, JSON-friendly order request covering all Alpaca
// order types and classes.
type OrderRequest struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"` // buy | sell
	Type     string  `json:"type"` // market | limit | stop | stop_limit | trailing_stop
	Qty      float64 `json:"qty"`
	Notional float64 `json:"notional"`

	LimitPrice   float64 `json:"limit_price"`   // limit, stop_limit
	StopPrice    float64 `json:"stop_price"`    // stop, stop_limit
	TrailPrice   float64 `json:"trail_price"`   // trailing_stop ($)
	TrailPercent float64 `json:"trail_percent"` // trailing_stop (%)

	TimeInForce   string `json:"time_in_force"` // day | gtc | ioc | fok | opg | cls
	ExtendedHours bool   `json:"extended_hours"`

	// Advanced classes: simple (default) | bracket | oco | oto.
	OrderClass      string  `json:"order_class"`
	TakeProfitLimit float64 `json:"take_profit_limit"` // take-profit leg limit price
	StopLossStop    float64 `json:"stop_loss_stop"`    // stop-loss leg stop price
	StopLossLimit   float64 `json:"stop_loss_limit"`   // optional stop-loss leg limit price
}

// Order is a JSON-friendly view of an Alpaca order.
type Order struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	Side        string    `json:"side"`
	Type        string    `json:"type"`
	Qty         string    `json:"qty"`
	Notional    string    `json:"notional"`
	FilledQty   string    `json:"filled_qty"`
	FilledPrice string    `json:"filled_avg_price"`
	LimitPrice  string    `json:"limit_price"`
	StopPrice   string    `json:"stop_price"`
	OrderClass  string    `json:"order_class"`
	TimeInForce string    `json:"time_in_force"`
	Status      string    `json:"status"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Position is a JSON-friendly view of an open position.
type Position struct {
	Symbol         string  `json:"symbol"`
	Qty            float64 `json:"qty"`
	Side           string  `json:"side"`
	AvgEntryPrice  float64 `json:"avg_entry_price"`
	CurrentPrice   float64 `json:"current_price"`
	MarketValue    float64 `json:"market_value"`
	CostBasis      float64 `json:"cost_basis"`
	UnrealizedPL   float64 `json:"unrealized_pl"`
	UnrealizedPLPC float64 `json:"unrealized_plpc"`
}

// Account is a JSON-friendly view of the account.
type Account struct {
	AccountNumber   string  `json:"account_number"`
	Status          string  `json:"status"`
	Currency        string  `json:"currency"`
	Equity          float64 `json:"equity"`
	LastEquity      float64 `json:"last_equity"`
	BuyingPower     float64 `json:"buying_power"`
	Cash            float64 `json:"cash"`
	PortfolioValue  float64 `json:"portfolio_value"`
	PatternDayTrader bool   `json:"pattern_day_trader"`
	TradingBlocked  bool    `json:"trading_blocked"`
}

// Asset captures the per-symbol metadata that drives the UI (notably whether
// dollar-amount / fractional orders are allowed).
type Asset struct {
	Symbol       string `json:"symbol"`
	Name         string `json:"name"`
	Class        string `json:"class"`
	Exchange     string `json:"exchange"`
	Tradable     bool   `json:"tradable"`
	Fractionable bool   `json:"fractionable"`
	Shortable    bool   `json:"shortable"`
}

// TradeUpdate is a normalized, real-time account order/fill event (from Alpaca's
// trade_updates stream).
type TradeUpdate struct {
	Event  string    `json:"event"`  // new | fill | partial_fill | canceled | rejected | ...
	Symbol string    `json:"symbol"`
	Side   string    `json:"side"`
	Qty    string    `json:"qty"`    // filled/affected quantity for this event
	Price  float64   `json:"price"`  // fill price (0 if not a fill)
	Status string    `json:"status"` // order status
	At     time.Time `json:"at"`
}

// Readiness reports whether the account can place live trades right now.
type Readiness struct {
	CanTrade             bool      `json:"can_trade"`
	AccountStatus        string    `json:"account_status"`
	Mode                 string    `json:"mode"`
	TradingBlocked       bool      `json:"trading_blocked"`
	AccountBlocked       bool      `json:"account_blocked"`
	TradeSuspendedByUser bool      `json:"trade_suspended_by_user"`
	ShortingEnabled      bool      `json:"shorting_enabled"`
	PatternDayTrader     bool      `json:"pattern_day_trader"`
	DaytradeCount        int64     `json:"daytrade_count"`
	BuyingPower          float64   `json:"buying_power"`
	Cash                 float64   `json:"cash"`
	Equity               float64   `json:"equity"`
	MarketOpen           bool      `json:"market_open"`
	NextOpen             time.Time `json:"next_open"`
	NextClose            time.Time `json:"next_close"`
	Issues               []string  `json:"issues"`
}

// Activity is a JSON-friendly fill/execution from the account activity log.
type Activity struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Symbol string    `json:"symbol"`
	Side   string    `json:"side"`
	Qty    float64   `json:"qty"`
	Price  float64   `json:"price"`
	Value  float64   `json:"value"`
	Type   string    `json:"type"`    // fill | partial_fill
	OrderID string   `json:"order_id"`
}

// KeyCheck reports the result of validating the configured credentials and the
// real-time SIP (Algo Trader Plus) data entitlement.
type KeyCheck struct {
	KeysValid       bool   `json:"keys_valid"`
	Mode            string `json:"mode"` // LIVE | PAPER
	AccountNumber   string `json:"account_number"`
	AccountStatus   string `json:"account_status"`
	TradingBlocked  bool   `json:"trading_blocked"`
	SIPEntitled     bool   `json:"sip_entitled"`
	ConfiguredFeed  string `json:"configured_feed"`
	Detail          string `json:"detail"`
}
