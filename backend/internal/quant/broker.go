// Package quant is, since 2026-07-31, ONLY the shared Alpaca paper-broker client.
//
// ⚠ READ BEFORE DELETING OR RENAMING THIS PACKAGE ⚠
// The AI quant pipeline this package was named for (agents, managers, allocator,
// signal trader, strategist, reviewer, clf gate, dip/rise desks) was REMOVED. What
// survives here is the plain REST order client that FIVE LIVE PAPER DESKS depend on:
//
//	internal/ridp  (+ ridp/guardian.go)  internal/rbt
//	internal/surger                      internal/breadcrumbs
//
// Deleting this package, or renaming it without updating those importers, breaks every
// one of them. Renaming to internal/paperbroker is a safe mechanical follow-up (a pure
// import-path change) — it was deliberately kept out of the removal commit so that no
// file holding live positions was touched. Nothing here talks to the real-money account.
package quant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// round2 rounds a price to cents. (Kept with the broker: order prices must be 2dp or
// Alpaca rejects them. Originally lived in the deleted quant/engine.go.)
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Broker is a paper-account order client (its OWN key pair, one per desk). It places real
// paper orders — market entries, trailing-stop floors, tightened stops, and market exits.
// The live real-money key is never used here.
type Broker struct {
	base   string
	key    string
	secret string
	http   *http.Client
}

func NewBroker(base, key, secret string) *Broker {
	return &Broker{base: strings.TrimRight(base, "/"), key: strings.TrimSpace(key), secret: strings.TrimSpace(secret), http: &http.Client{Timeout: 12 * time.Second}}
}

func (b *Broker) Enabled() bool { return b != nil && b.key != "" && b.secret != "" }

func (b *Broker) do(method, path string, payload interface{}) ([]byte, int, error) {
	var buf []byte
	if payload != nil {
		buf, _ = json.Marshal(payload)
	}
	attempt := func() ([]byte, int, error) {
		var body io.Reader
		if buf != nil {
			body = bytes.NewReader(buf)
		}
		req, err := http.NewRequest(method, b.base+path, body)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("APCA-API-KEY-ID", b.key)
		req.Header.Set("APCA-API-SECRET-KEY", b.secret)
		if buf != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := b.http.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
		return rb, resp.StatusCode, nil
	}
	rb, code, err := attempt()
	// One retry on rate limiting: a 429 means the request was NOT processed, so any
	// call (including order posts — coids dedupe anyway) is safe to send again after a
	// breath. Prevents a single burst from cascading into failed fills/exits.
	if err == nil && code == http.StatusTooManyRequests {
		time.Sleep(500 * time.Millisecond)
		rb, code, err = attempt()
	}
	return rb, code, err
}

// AccountInfo is the paper account's live capital snapshot.
type AccountInfo struct {
	Cash        float64
	BuyingPower float64
	Equity      float64 // portfolio value (cash + positions)
	LastEquity  float64 // equity at the prior trading day's close (Alpaca's number)
}

// DayPnL is Alpaca's own day profit-and-loss: today's equity vs yesterday's close.
// Broker-level truth — it includes every share on the account, tracked or not.
func (a AccountInfo) DayPnL() float64 {
	if a.LastEquity <= 0 {
		return 0
	}
	return a.Equity - a.LastEquity
}

// Account fetches the paper account's real cash / buying power / equity so the allocator
// can cap its budget at money that actually exists (rather than assuming a fixed number).
func (b *Broker) Account() (AccountInfo, error) {
	rb, code, err := b.do(http.MethodGet, "/account", nil)
	if err != nil {
		return AccountInfo{}, err
	}
	if code != http.StatusOK {
		return AccountInfo{}, fmt.Errorf("account (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var a struct {
		Cash           string `json:"cash"`
		BuyingPower    string `json:"buying_power"`
		Equity         string `json:"equity"`
		PortfolioValue string `json:"portfolio_value"`
		LastEquity     string `json:"last_equity"`
	}
	if err := json.Unmarshal(rb, &a); err != nil {
		return AccountInfo{}, err
	}
	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	eq := pf(a.Equity)
	if eq == 0 {
		eq = pf(a.PortfolioValue)
	}
	return AccountInfo{Cash: pf(a.Cash), BuyingPower: pf(a.BuyingPower), Equity: eq, LastEquity: pf(a.LastEquity)}, nil
}

func (b *Broker) order(payload map[string]interface{}) (string, error) {
	rb, code, err := b.do(http.MethodPost, "/orders", payload)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated && code != http.StatusAccepted {
		return "", fmt.Errorf("order rejected (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var or struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rb, &or)
	return or.ID, nil
}

// MarketBuy buys whole shares at market (entries are whole-share so the protective stop is valid).
func (b *Broker) MarketBuy(sym string, qty float64, coid string) (string, error) {
	return b.order(map[string]interface{}{
		"symbol": sym, "qty": wholeQty(qty), "side": "buy", "type": "market",
		"time_in_force": "day", "client_order_id": coid,
	})
}

// MarketSell sells whole shares at market (Agent-3 exits).
func (b *Broker) MarketSell(sym string, qty float64, coid string) (string, error) {
	return b.order(map[string]interface{}{
		"symbol": sym, "qty": wholeQty(qty), "side": "sell", "type": "market",
		"time_in_force": "day", "client_order_id": coid,
	})
}

// TrailingStopSell places the deterministic protective floor: a trailing stop that follows price
// up by trailPct and triggers on a pullback. GTC so it rests until filled or replaced.
func (b *Broker) TrailingStopSell(sym string, qty, trailPct float64, coid string) (string, error) {
	return b.order(map[string]interface{}{
		"symbol": sym, "qty": wholeQty(qty), "side": "sell", "type": "trailing_stop",
		"trail_percent": trailPct, "time_in_force": "gtc", "client_order_id": coid,
	})
}

// StopSell places a fixed stop (used when Agent 3 ratchets the floor up to a specific price).
func (b *Broker) StopSell(sym string, qty, stopPrice float64, coid string) (string, error) {
	return b.order(map[string]interface{}{
		"symbol": sym, "qty": wholeQty(qty), "side": "sell", "type": "stop",
		"stop_price": round2(stopPrice), "time_in_force": "gtc", "client_order_id": coid,
	})
}

// StopBuy places a fixed buy stop (used to protect short positions).
func (b *Broker) StopBuy(sym string, qty, stopPrice float64, coid string) (string, error) {
	return b.order(map[string]interface{}{
		"symbol": sym, "qty": wholeQty(qty), "side": "buy", "type": "stop",
		"stop_price": round2(stopPrice), "time_in_force": "gtc", "client_order_id": coid,
	})
}

// CancelOpenOrders cancels every OPEN order for one symbol. A standalone stop is NOT
// auto-canceled when its position closes (only bracket/OCO siblings are), so this is used to
// clear an orphaned protective stop after a position is closed outside the normal exit path,
// and to guarantee exactly one valid stop when rehydrating after a restart.
func (b *Broker) CancelOpenOrders(sym string) error {
	rb, code, err := b.do(http.MethodGet, "/orders?status=open&limit=100&symbols="+url.QueryEscape(sym), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("open orders (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var ords []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rb, &ords); err != nil {
		return err
	}
	for _, o := range ords {
		_ = b.Cancel(o.ID)
	}
	return nil
}

// Cancel cancels one order (e.g. the old stop before placing a tighter one, or before a market exit).
func (b *Broker) Cancel(id string) error {
	if id == "" {
		return nil
	}
	_, code, err := b.do(http.MethodDelete, "/orders/"+id, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNoContent && code != http.StatusAccepted {
		return fmt.Errorf("cancel %s failed (%d)", id, code)
	}
	return nil
}

// Order returns an order's fill state (filled qty, avg price, status).
func (b *Broker) Order(id string) (filledQty, avgPrice float64, status string, err error) {
	rb, code, err := b.do(http.MethodGet, "/orders/"+id, nil)
	if err != nil {
		return 0, 0, "", err
	}
	if code != http.StatusOK {
		return 0, 0, "", fmt.Errorf("get order (%d)", code)
	}
	var o paperOrd
	if err := json.Unmarshal(rb, &o); err != nil {
		return 0, 0, "", err
	}
	fq, _ := strconv.ParseFloat(o.FilledQty, 64)
	ap, _ := strconv.ParseFloat(o.FilledAvgPrice, 64)
	return fq, ap, o.Status, nil
}

// OrderByCoid resolves an order by its client_order_id — used to settle in-flight orders
// whose server id was never learned (a crash between the POST and its response). Returns
// id=="" with nil error when no such order exists (the POST never reached Alpaca).
func (b *Broker) OrderByCoid(coid string) (id string, filledQty, avgPrice float64, status string, err error) {
	rb, code, err := b.do(http.MethodGet, "/orders:by_client_order_id?client_order_id="+url.QueryEscape(coid), nil)
	if err != nil {
		return "", 0, 0, "", err
	}
	if code == http.StatusNotFound {
		return "", 0, 0, "", nil
	}
	if code != http.StatusOK {
		return "", 0, 0, "", fmt.Errorf("order by coid (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var o paperOrd
	if err := json.Unmarshal(rb, &o); err != nil {
		return "", 0, 0, "", err
	}
	fq, _ := strconv.ParseFloat(o.FilledQty, 64)
	ap, _ := strconv.ParseFloat(o.FilledAvgPrice, 64)
	return o.ID, fq, ap, o.Status, nil
}

// PositionQty returns how many shares of sym the paper account currently holds (0 if none).
func (b *Broker) PositionQty(sym string) (float64, error) {
	rb, code, err := b.do(http.MethodGet, "/positions/"+sym, nil)
	if err != nil {
		return 0, err
	}
	if code == http.StatusNotFound {
		return 0, nil
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("get position (%d)", code)
	}
	var p struct {
		Qty string `json:"qty"`
	}
	_ = json.Unmarshal(rb, &p)
	q, _ := strconv.ParseFloat(p.Qty, 64)
	return q, nil
}

type paperOrd struct {
	ID             string     `json:"id"`
	ClientOrderID  string     `json:"client_order_id"`
	Symbol         string     `json:"symbol"`
	Side           string     `json:"side"`
	Type           string     `json:"type"`
	Qty            string     `json:"qty"`
	StopPrice      string     `json:"stop_price"`
	FilledQty      string     `json:"filled_qty"`
	FilledAvgPrice string     `json:"filled_avg_price"`
	Status         string     `json:"status"`
	FilledAt       *time.Time `json:"filled_at"`
	SubmittedAt    *time.Time `json:"submitted_at"`
}

// BrokerPosition is one open position on the paper account.
type BrokerPosition struct {
	Symbol   string
	Qty      float64
	AvgEntry float64
	// CurrentPx is Alpaca's own mark for the position (additive 2026-07-24: lets desks
	// price positions the candle engine doesn't stream, e.g. adopted names off-hours).
	CurrentPx float64
}

// Positions lists the paper account's open positions (the account is dedicated to the
// quant desk, so every position here is ours).
func (b *Broker) Positions() ([]BrokerPosition, error) {
	rb, code, err := b.do(http.MethodGet, "/positions", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("positions fetch (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var raw []struct {
		Symbol        string `json:"symbol"`
		Qty           string `json:"qty"`
		AvgEntryPrice string `json:"avg_entry_price"`
		CurrentPrice  string `json:"current_price"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]BrokerPosition, 0, len(raw))
	for _, p := range raw {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		ae, _ := strconv.ParseFloat(p.AvgEntryPrice, 64)
		cp, _ := strconv.ParseFloat(p.CurrentPrice, 64)
		out = append(out, BrokerPosition{Symbol: p.Symbol, Qty: q, AvgEntry: ae, CurrentPx: cp})
	}
	return out, nil
}

// OpenOrder is one resting order on the account (id + routing essentials only).
type OpenOrder struct {
	ID     string
	Symbol string
	Side   string
}

// OpenOrders lists the account's open orders (additive 2026-07-25: lets desks cancel a
// symbol's ORPHANED resting orders before flattening ghost shares — selling shares an
// old stop still holds is the "insufficient qty available" 403 class, 64 failures on
// RIDP 07-20).
func (b *Broker) OpenOrders() ([]OpenOrder, error) {
	rb, code, err := b.do(http.MethodGet, "/orders?status=open&limit=500", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("open orders fetch (%d): %s", code, strings.TrimSpace(string(rb)))
	}
	var raw []struct {
		ID     string `json:"id"`
		Symbol string `json:"symbol"`
		Side   string `json:"side"`
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]OpenOrder, 0, len(raw))
	for _, o := range raw {
		out = append(out, OpenOrder{ID: o.ID, Symbol: o.Symbol, Side: o.Side})
	}
	return out, nil
}

func wholeQty(q float64) string { return strconv.FormatFloat(math.Floor(q), 'f', 0, 64) }
