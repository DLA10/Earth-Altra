package quant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// QuantStrategy prefixes every client_order_id so the quant pipeline's orders are
// distinguishable from anything else on the (shared) paper account during reconstruction.
const QuantStrategy = "QuantDip"

// Broker is the quant pipeline's paper-account order client (its OWN key pair). It places real
// paper orders — market entries, the deterministic trailing-stop floor, tightened stops, and
// market exits — and reconstructs P&L from the account's order history. The live real-money key
// is never used here.
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
	var body io.Reader
	if payload != nil {
		buf, _ := json.Marshal(payload)
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("APCA-API-KEY-ID", b.key)
	req.Header.Set("APCA-API-SECRET-KEY", b.secret)
	if payload != nil {
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

// AccountInfo is the paper account's live capital snapshot.
type AccountInfo struct {
	Cash        float64
	BuyingPower float64
	Equity      float64 // portfolio value (cash + positions)
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
	}
	if err := json.Unmarshal(rb, &a); err != nil {
		return AccountInfo{}, err
	}
	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	eq := pf(a.Equity)
	if eq == 0 {
		eq = pf(a.PortfolioValue)
	}
	return AccountInfo{Cash: pf(a.Cash), BuyingPower: pf(a.BuyingPower), Equity: eq}, nil
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

// allOrders fetches the account's full order history, paginating past Alpaca's 500-per-
// request cap (ascending by submitted_at with an exclusive `after` cursor), so
// reconstruction doesn't silently lose the oldest entries — and mispair entries with
// exits — once the account's lifetime order count passes one page.
func (b *Broker) allOrders() ([]paperOrd, error) {
	var out []paperOrd
	after := ""
	for page := 0; page < 40; page++ { // 40 × 500 = 20k orders — runaway guard
		path := "/orders?status=all&limit=500&nested=false&direction=asc"
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		rb, code, err := b.do(http.MethodGet, path, nil)
		if err != nil {
			return out, err
		}
		if code != http.StatusOK {
			return out, fmt.Errorf("orders fetch (%d): %s", code, strings.TrimSpace(string(rb)))
		}
		var batch []paperOrd
		if err := json.Unmarshal(rb, &batch); err != nil {
			return out, err
		}
		out = append(out, batch...)
		if len(batch) < 500 {
			break
		}
		last := batch[len(batch)-1]
		if last.SubmittedAt == nil {
			break
		}
		after = last.SubmittedAt.Format(time.RFC3339Nano)
	}
	return out, nil
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
	}
	if err := json.Unmarshal(rb, &raw); err != nil {
		return nil, err
	}
	out := make([]BrokerPosition, 0, len(raw))
	for _, p := range raw {
		q, _ := strconv.ParseFloat(p.Qty, 64)
		ae, _ := strconv.ParseFloat(p.AvgEntryPrice, 64)
		out = append(out, BrokerPosition{Symbol: p.Symbol, Qty: q, AvgEntry: ae})
	}
	return out, nil
}

func ordTime(o paperOrd) time.Time {
	if o.FilledAt != nil {
		return *o.FilledAt
	}
	if o.SubmittedAt != nil {
		return *o.SubmittedAt
	}
	return time.Now()
}

// Reconstruct rebuilds the quant pipeline's positions + closed trades + realized P&L from its own
// filled paper orders (prefix QuantStrategy), pairing entries with the next sell per symbol. Any
// filled sell (Agent-3 market exit OR the protective stop) closes the position; the exit reason
// comes from the sell's client_order_id. markFn marks open positions to the live price.
func (b *Broker) Reconstruct(markFn func(string) float64) (QuantState, error) {
	raw, err := b.allOrders()
	if err != nil {
		return QuantState{}, err
	}

	filled := make([]paperOrd, 0, len(raw))
	for _, o := range raw {
		if !strings.HasPrefix(o.ClientOrderID, QuantStrategy+"__") {
			continue
		}
		fq, _ := strconv.ParseFloat(o.FilledQty, 64)
		fp, _ := strconv.ParseFloat(o.FilledAvgPrice, 64)
		if fq <= 0 || fp <= 0 {
			continue
		}
		// Only fully-filled orders close/open a position cleanly. (Market orders fill fully;
		// counting partials here would mispair entries/exits and misstate realized P&L.)
		if o.Status == "filled" {
			filled = append(filled, o)
		}
	}
	sort.Slice(filled, func(i, j int) bool { return ordTime(filled[i]).Before(ordTime(filled[j])) })

	var st QuantState
	open := map[string]QuantPosition{}
	for _, o := range filled {
		parts := strings.Split(o.ClientOrderID, "__")
		if len(parts) < 3 {
			continue
		}
		sym := parts[1]
		qty, _ := strconv.ParseFloat(o.FilledQty, 64)
		price, _ := strconv.ParseFloat(o.FilledAvgPrice, 64)
		t := ordTime(o)
		if o.Side == "buy" {
			open[sym] = QuantPosition{Symbol: sym, Qty: qty, EntryPrice: price, EntryTime: t, MarkPrice: price}
			continue
		}
		// Any sell closes the position. Reason = parts[3] if present (entry/exit/stop coids),
		// else "Trail_Stop" (a protective stop fill named without a reason segment).
		reason := "Trail_Stop"
		if len(parts) >= 4 && parts[3] != "" {
			reason = parts[3]
		}
		if pos, ok := open[sym]; ok {
			st.Trades = append(st.Trades, QuantTrade{
				Symbol: sym, EntryTime: pos.EntryTime, ExitTime: t,
				EntryPrice: pos.EntryPrice, ExitPrice: price, Qty: pos.Qty,
				PNL: pos.Qty * (price - pos.EntryPrice), ExitReason: reason,
			})
			st.RealizedPNL += pos.Qty * (price - pos.EntryPrice)
			delete(open, sym)
		}
	}
	for _, pos := range open {
		if markFn != nil {
			if px := markFn(pos.Symbol); px > 0 {
				pos.MarkPrice = px
				pos.UnrealizedPNL = pos.Qty * (px - pos.EntryPrice)
			}
		}
		st.UnrealizedPNL += pos.UnrealizedPNL
		st.Positions = append(st.Positions, pos)
	}
	sort.Slice(st.Positions, func(i, j int) bool { return st.Positions[i].Symbol < st.Positions[j].Symbol })
	wins := 0
	for _, tr := range st.Trades {
		if tr.PNL > 0 {
			wins++
		}
	}
	st.TotalTrades = len(st.Trades)
	if st.TotalTrades > 0 {
		st.WinRate = float64(wins) / float64(st.TotalTrades) * 100
	}
	return st, nil
}

func wholeQty(q float64) string { return strconv.FormatFloat(math.Floor(q), 'f', 0, 64) }
