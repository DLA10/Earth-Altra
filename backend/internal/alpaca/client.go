// Package alpaca wraps the official Alpaca Go SDK with our own JSON-friendly DTOs,
// keeping the HTTP/WS layers decoupled from the SDK's decimal types.
package alpaca

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	api "github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata/stream"
	"github.com/shopspring/decimal"

	"live-optimus/backend/internal/config"
)

// Client bundles the trading and market-data clients.
type Client struct {
	trading *api.Client
	data    *marketdata.Client
	feed    marketdata.Feed
	cfg     *config.Config

	// Live stream handle + handlers, retained so symbols can be subscribed at runtime.
	streamMu sync.RWMutex
	stockStream *stream.StocksClient
	tradeCb     func(stream.Trade)
	quoteCb     func(stream.Quote)

	// Cached list of tradable US-equity assets, for the symbol search.
	assetMu    sync.RWMutex
	assetCache []Asset
	assetAt    time.Time
}

// New constructs a Client from config.
func New(cfg *config.Config) *Client {
	trading := api.NewClient(api.ClientOpts{
		APIKey:    cfg.APIKeyID,
		APISecret: cfg.APISecret,
		BaseURL:   cfg.TradingBaseURL,
	})
	data := marketdata.NewClient(marketdata.ClientOpts{
		APIKey:    cfg.APIKeyID,
		APISecret: cfg.APISecret,
		BaseURL:   cfg.DataBaseURL,
	})
	return &Client{
		trading: trading,
		data:    data,
		feed:    feedFromString(cfg.DataFeed),
		cfg:     cfg,
	}
}

func feedFromString(s string) marketdata.Feed {
	switch strings.ToLower(s) {
	case "iex":
		return marketdata.IEX
	case "otc":
		return marketdata.OTC
	default:
		return marketdata.SIP
	}
}

// VerifyKeys validates credentials and probes whether the configured feed (SIP for
// Algo Trader Plus) is entitled. A subscription-not-permitted error on a SIP bar
// request means the Algo Trader Plus upgrade has not taken effect.
func (c *Client) VerifyKeys() KeyCheck {
	out := KeyCheck{Mode: c.cfg.Mode(), ConfiguredFeed: c.cfg.DataFeed}

	acct, err := c.trading.GetAccount()
	if err != nil {
		out.Detail = fmt.Sprintf("account lookup failed: %v", err)
		return out
	}
	out.KeysValid = true
	out.AccountNumber = acct.AccountNumber
	out.AccountStatus = string(acct.Status)
	out.TradingBlocked = acct.TradingBlocked

	// Probe SIP entitlement with a small recent bar request.
	end := time.Now().Add(-2 * time.Minute)
	start := end.Add(-30 * time.Minute)
	_, err = c.data.GetBars("AAPL", marketdata.GetBarsRequest{
		TimeFrame: marketdata.OneMin,
		Start:     start,
		End:       end,
		Feed:      marketdata.SIP,
		TotalLimit: 1,
	})
	if err != nil {
		out.SIPEntitled = false
		out.Detail = fmt.Sprintf("SIP probe failed (likely not subscribed to Algo Trader Plus): %v", err)
		return out
	}
	out.SIPEntitled = true
	out.Detail = "Keys valid; SIP real-time data entitled (Algo Trader Plus active)."
	return out
}

// GetAccount returns account snapshot.
func (c *Client) GetAccount() (*Account, error) {
	a, err := c.trading.GetAccount()
	if err != nil {
		return nil, err
	}
	return &Account{
		AccountNumber:    a.AccountNumber,
		Status:           string(a.Status),
		Currency:         a.Currency,
		Equity:           df(a.Equity),
		LastEquity:       df(a.LastEquity),
		BuyingPower:      df(a.BuyingPower),
		Cash:             df(a.Cash),
		PortfolioValue:   df(a.PortfolioValue),
		PatternDayTrader: a.PatternDayTrader,
		TradingBlocked:   a.TradingBlocked,
	}, nil
}

// GetPositions returns all open positions.
func (c *Client) GetPositions() ([]Position, error) {
	ps, err := c.trading.GetPositions()
	if err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(ps))
	for _, p := range ps {
		out = append(out, Position{
			Symbol:         p.Symbol,
			Qty:            df(p.Qty),
			Side:           string(p.Side),
			AvgEntryPrice:  df(p.AvgEntryPrice),
			CurrentPrice:   dpf(p.CurrentPrice),
			MarketValue:    dpf(p.MarketValue),
			CostBasis:      df(p.CostBasis),
			UnrealizedPL:   dpf(p.UnrealizedPL),
			UnrealizedPLPC: dpf(p.UnrealizedPLPC),
		})
	}
	return out, nil
}

// GetOpenOrders returns open orders.
func (c *Client) GetOpenOrders() ([]Order, error) {
	status := "open"
	orders, err := c.trading.GetOrders(api.GetOrdersRequest{Status: status, Nested: true})
	if err != nil {
		return nil, err
	}
	out := make([]Order, 0, len(orders))
	for _, o := range orders {
		out = append(out, toOrder(o))
		// Flatten legs (e.g. OCO take-profit + stop-loss, bracket exits) so each resting leg
		// is visible on its own — the chart can draw each as its own line.
		for _, leg := range o.Legs {
			out = append(out, toOrder(leg))
		}
	}
	return out, nil
}

// GetAsset returns asset metadata (used for the fractionable check).
func (c *Client) GetAsset(symbol string) (*Asset, error) {
	a, err := c.trading.GetAsset(symbol)
	if err != nil {
		return nil, err
	}
	return &Asset{
		Symbol:       a.Symbol,
		Name:         a.Name,
		Class:        string(a.Class),
		Exchange:     a.Exchange,
		Tradable:     a.Tradable,
		Fractionable: a.Fractionable,
		Shortable:    a.Shortable,
	}, nil
}

// loadAssets returns the cached list of tradable US-equity assets, refreshing it from
// Alpaca if the cache is empty or stale (>12h). The full list is ~10k symbols; it's
// fetched once and searched in memory, so per-keystroke searches cost no Alpaca calls.
func (c *Client) loadAssets() ([]Asset, error) {
	c.assetMu.RLock()
	if len(c.assetCache) > 0 && time.Since(c.assetAt) < 12*time.Hour {
		cached := c.assetCache
		c.assetMu.RUnlock()
		return cached, nil
	}
	c.assetMu.RUnlock()

	raw, err := c.trading.GetAssets(api.GetAssetsRequest{Status: "active", AssetClass: "us_equity"})
	if err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(raw))
	for _, a := range raw {
		if !a.Tradable {
			continue
		}
		out = append(out, Asset{
			Symbol:       a.Symbol,
			Name:         a.Name,
			Class:        string(a.Class),
			Exchange:     a.Exchange,
			Tradable:     a.Tradable,
			Fractionable: a.Fractionable,
			Shortable:    a.Shortable,
		})
	}
	c.assetMu.Lock()
	c.assetCache = out
	c.assetAt = time.Now()
	c.assetMu.Unlock()
	return out, nil
}

// SearchAssets finds tradable US equities whose symbol or company name matches the
// query, ranked: exact symbol, then symbol-prefix, then name-contains, then
// symbol-contains. Returns up to `limit` results.
func (c *Client) SearchAssets(query string, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	q := strings.ToUpper(strings.TrimSpace(query))
	if q == "" {
		return []Asset{}, nil
	}
	assets, err := c.loadAssets()
	if err != nil {
		return nil, err
	}
	type scored struct {
		a    Asset
		rank int
	}
	var matches []scored
	for _, a := range assets {
		nameU := strings.ToUpper(a.Name)
		rank := -1
		switch {
		case a.Symbol == q:
			rank = 0
		case strings.HasPrefix(a.Symbol, q):
			rank = 1
		case strings.Contains(nameU, q):
			rank = 2
		case strings.Contains(a.Symbol, q):
			rank = 3
		}
		if rank >= 0 {
			matches = append(matches, scored{a, rank})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return matches[i].a.Symbol < matches[j].a.Symbol
	})
	out := make([]Asset, 0, limit)
	for _, m := range matches {
		if len(out) >= limit {
			break
		}
		out = append(out, m.a)
	}
	return out, nil
}

// PlaceOrder submits an order. Caller is responsible for validation; this maps all
// supported order types and classes (simple/bracket/oco/oto, stops, trailing, GTC,
// extended hours) to the Alpaca request.
func (c *Client) PlaceOrder(r OrderRequest) (*Order, error) {
	tif := api.TimeInForce(strings.ToLower(r.TimeInForce))
	if tif == "" {
		tif = api.Day
	}
	req := api.PlaceOrderRequest{
		Symbol:        r.Symbol,
		Side:          api.Side(strings.ToLower(r.Side)),
		Type:          api.OrderType(strings.ToLower(r.Type)),
		TimeInForce:   tif,
		ExtendedHours: r.ExtendedHours,
	}
	if oc := strings.ToLower(r.OrderClass); oc != "" && oc != "simple" {
		req.OrderClass = api.OrderClass(oc)
	}
	if r.Notional > 0 {
		req.Notional = decPtr(r.Notional)
	} else {
		req.Qty = decPtr(r.Qty)
	}
	if r.LimitPrice > 0 {
		req.LimitPrice = decPtr(r.LimitPrice)
	}
	if r.StopPrice > 0 {
		req.StopPrice = decPtr(r.StopPrice)
	}
	if r.TrailPrice > 0 {
		req.TrailPrice = decPtr(r.TrailPrice)
	}
	if r.TrailPercent > 0 {
		req.TrailPercent = decPtr(r.TrailPercent)
	}
	if r.TakeProfitLimit > 0 {
		req.TakeProfit = &api.TakeProfit{LimitPrice: decPtr(r.TakeProfitLimit)}
	}
	if r.StopLossStop > 0 {
		sl := &api.StopLoss{StopPrice: decPtr(r.StopLossStop)}
		if r.StopLossLimit > 0 {
			sl.LimitPrice = decPtr(r.StopLossLimit)
		}
		req.StopLoss = sl
	}

	o, err := c.trading.PlaceOrder(req)
	if err != nil {
		return nil, err
	}
	order := toOrder(*o)
	return &order, nil
}

// Readiness reports whether the account can place live trades right now.
func (c *Client) Readiness() (*Readiness, error) {
	a, err := c.trading.GetAccount()
	if err != nil {
		return nil, err
	}
	out := &Readiness{
		AccountStatus:        string(a.Status),
		Mode:                 c.cfg.Mode(),
		TradingBlocked:       a.TradingBlocked,
		AccountBlocked:       a.AccountBlocked,
		TradeSuspendedByUser: a.TradeSuspendedByUser,
		ShortingEnabled:      a.ShortingEnabled,
		PatternDayTrader:     a.PatternDayTrader,
		DaytradeCount:        a.DaytradeCount,
		BuyingPower:          df(a.BuyingPower),
		Cash:                 df(a.Cash),
		Equity:               df(a.Equity),
	}
	// Market clock is best-effort.
	if clk, err := c.trading.GetClock(); err == nil {
		out.MarketOpen = clk.IsOpen
		out.NextOpen = clk.NextOpen
		out.NextClose = clk.NextClose
	}

	var issues []string
	if string(a.Status) != "ACTIVE" {
		issues = append(issues, "account status is "+string(a.Status)+" (not ACTIVE)")
	}
	if a.TradingBlocked {
		issues = append(issues, "trading is blocked on this account")
	}
	if a.AccountBlocked {
		issues = append(issues, "account is blocked")
	}
	if a.TradeSuspendedByUser {
		issues = append(issues, "trading is suspended by user setting")
	}
	if df(a.BuyingPower) <= 0 {
		issues = append(issues, "no buying power — deposit funds to trade")
	}
	out.Issues = issues
	out.CanTrade = len(issues) == 0
	return out, nil
}

func decPtr(f float64) *decimal.Decimal {
	d := decimal.NewFromFloat(f)
	return &d
}

// CancelOrder cancels a single order by ID.
func (c *Client) CancelOrder(id string) error {
	return c.trading.CancelOrder(id)
}

// CancelAllOrders cancels all open orders (kill switch).
func (c *Client) CancelAllOrders() error {
	return c.trading.CancelAllOrders()
}

// GetFills returns the account's fill activities (the authoritative trade log),
// most recent first, since `after` (zero = let Alpaca return the most recent page).
func (c *Client) GetFills(after time.Time, limit int) ([]Activity, error) {
	// Alpaca caps activity page size at 100.
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	req := api.GetAccountActivitiesRequest{
		ActivityTypes: []string{"FILL"},
		Direction:     "desc",
		PageSize:      limit,
	}
	if !after.IsZero() {
		req.After = after
	}
	acts, err := c.trading.GetAccountActivities(req)
	if err != nil {
		return nil, err
	}
	out := make([]Activity, 0, len(acts))
	for _, a := range acts {
		qty := df(a.Qty)
		price := df(a.Price)
		out = append(out, Activity{
			ID:      a.ID,
			Time:    a.TransactionTime,
			Symbol:  a.Symbol,
			Side:    a.Side,
			Qty:     qty,
			Price:   price,
			Value:   qty * price,
			Type:    a.Type,
			OrderID: a.OrderID,
		})
	}
	return out, nil
}

// GetAllFills returns every FILL activity since `after`, paginating past Alpaca's
// 100-per-page cap (used by the Metrics page, which needs a full day/week/month of
// fills). Ascending by time so callers can walk positions chronologically.
func (c *Client) GetAllFills(after time.Time) ([]Activity, error) {
	var out []Activity
	pageToken := ""
	for page := 0; page < 100; page++ { // hard cap (100*100=10k fills) as a runaway guard
		req := api.GetAccountActivitiesRequest{
			ActivityTypes: []string{"FILL"},
			Direction:     "asc",
			PageSize:      100,
			PageToken:     pageToken,
		}
		if !after.IsZero() {
			req.After = after
		}
		acts, err := c.trading.GetAccountActivities(req)
		if err != nil {
			return out, err
		}
		if len(acts) == 0 {
			break
		}
		for _, a := range acts {
			qty := df(a.Qty)
			price := df(a.Price)
			out = append(out, Activity{
				ID:      a.ID,
				Time:    a.TransactionTime,
				Symbol:  a.Symbol,
				Side:    a.Side,
				Qty:     qty,
				Price:   price,
				Value:   qty * price,
				Type:    a.Type,
				OrderID: a.OrderID,
			})
		}
		if len(acts) < 100 {
			break
		}
		pageToken = acts[len(acts)-1].ID
	}
	return out, nil
}

// StreamTradeUpdates streams account order/fill events in real time. It runs in the
// background and reconnects automatically until ctx is cancelled.
func (c *Client) StreamTradeUpdates(ctx context.Context, handler func(TradeUpdate)) {
	c.trading.StreamTradeUpdatesInBackground(ctx, func(tu api.TradeUpdate) {
		out := TradeUpdate{
			Event:  tu.Event,
			Symbol: tu.Order.Symbol,
			Side:   string(tu.Order.Side),
			Status: string(tu.Order.Status),
			At:     tu.At,
			Price:  dpf(tu.Price),
		}
		if tu.Qty != nil {
			out.Qty = tu.Qty.String()
		}
		handler(out)
	})
}

// --- helpers ---

func toOrder(o api.Order) Order {
	return Order{
		ID:          o.ID,
		Symbol:      o.Symbol,
		Side:        string(o.Side),
		Type:        string(o.Type),
		Qty:         dstr(o.Qty),
		Notional:    dpstr(o.Notional),
		FilledQty:   dstr2(o.FilledQty),
		FilledPrice: dpstr(o.FilledAvgPrice),
		LimitPrice:  dpstr(o.LimitPrice),
		StopPrice:   dpstr(o.StopPrice),
		OrderClass:  string(o.OrderClass),
		TimeInForce: string(o.TimeInForce),
		Status:      string(o.Status),
		SubmittedAt: o.SubmittedAt,
	}
}

// df converts a decimal.Decimal to float64.
func df(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

// dpf converts a *decimal.Decimal to float64 (0 if nil).
func dpf(d *decimal.Decimal) float64 {
	if d == nil {
		return 0
	}
	f, _ := d.Float64()
	return f
}

func dstr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

func dstr2(d decimal.Decimal) string {
	return d.String()
}

func dpstr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}
