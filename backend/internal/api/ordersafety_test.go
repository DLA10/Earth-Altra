package api

import (
	"testing"
	"time"

	"live-optimus/backend/internal/alpaca"
	"live-optimus/backend/internal/candles"
	"live-optimus/backend/internal/config"
)

func testServer(price float64) *Server {
	eng := candles.NewEngine([]string{"AAA"}, 100)
	eng.Seed("AAA", []candles.Candle{{Time: time.Now().Unix() - 60, Open: price, High: price, Low: price, Close: price, Volume: 100}})
	return &Server{
		Cfg:          &config.Config{MaxOrderNotional: 25000},
		Engine:       eng,
		Fractionable: map[string]bool{"AAA": true},
	}
}

func mustReject(t *testing.T, s *Server, r alpaca.OrderRequest, why string) {
	t.Helper()
	if err := s.validateOrder(r); err == nil {
		t.Fatalf("expected REJECT (%s) but it passed", why)
	}
}
func mustAccept(t *testing.T, s *Server, r alpaca.OrderRequest, why string) {
	t.Helper()
	if err := s.validateOrder(r); err != nil {
		t.Fatalf("expected ACCEPT (%s) but got: %v", why, err)
	}
}

func TestCap_MarketBuyByShares(t *testing.T) {
	s := testServer(500) // $500/share
	// THE GAP THE AUDIT FOUND: market buy 100 sh = $50k must now be capped (was slipping through).
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", Qty: 100}, "100sh @ $500 = $50k > $25k cap")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", Qty: 10}, "10sh @ $500 = $5k < cap")
}

func TestCap_SellsNotBlocked(t *testing.T) {
	s := testServer(500)
	// Protective/large SELLS must NOT be cap-blocked (checkSellable bounds them).
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "market", Qty: 100}, "large sell not capped")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "stop", Qty: 100, StopPrice: 480}, "large stop-loss not capped")
}

func TestDirectionGuard(t *testing.T) {
	s := testServer(100) // $100/share
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", Qty: 1, LimitPrice: 150}, "buy-limit 50% above market")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", Qty: 1, LimitPrice: 95}, "buy-the-dip below market")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "stop", Qty: 1, StopPrice: 150}, "sell-stop above market (wrong side)")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "stop", Qty: 1, StopPrice: 95}, "normal stop-loss below market")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", Qty: 1, LimitPrice: 100.5}, "marketable-by-design within 3% band passes")
}

func TestOCODirection(t *testing.T) {
	s := testServer(100)
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "limit", OrderClass: "oco", Qty: 1, TakeProfitLimit: 90, StopLossStop: 80}, "OCO take-profit below market")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "limit", OrderClass: "oco", Qty: 1, TakeProfitLimit: 110, StopLossStop: 105}, "OCO stop-loss above market")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "sell", Type: "limit", OrderClass: "oco", Qty: 1, TakeProfitLimit: 110, StopLossStop: 90}, "proper OCO: TP above, SL below")
}

func TestBracket(t *testing.T) {
	s := testServer(100)
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", OrderClass: "bracket", TimeInForce: "day", Qty: 10, TakeProfitLimit: 110, StopLossStop: 90}, "proper market bracket")
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", OrderClass: "bracket", TimeInForce: "gtc", Qty: 10, LimitPrice: 95, TakeProfitLimit: 110, StopLossStop: 90}, "proper limit bracket (buy the dip)")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", OrderClass: "bracket", TimeInForce: "day", Qty: 10, TakeProfitLimit: 90, StopLossStop: 80}, "bracket take-profit below market")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", OrderClass: "bracket", TimeInForce: "day", Qty: 10, TakeProfitLimit: 110, StopLossStop: 105}, "bracket stop-loss above market")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "market", OrderClass: "bracket", TimeInForce: "day", Qty: 300, TakeProfitLimit: 110, StopLossStop: 90}, "oversized bracket capped (300 sh @ $100 = $30k)")
}

func TestBracketLimitEntryLegsBoundEntryNotMarket(t *testing.T) {
	s := testServer(100)
	// A limit-entry bracket's TP/SL bound the ENTRY price, not the current market price:
	// stock at $100, buy at $95, TP $98 / SL $93 is a perfectly valid trade plan.
	mustAccept(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", OrderClass: "bracket", TimeInForce: "gtc", Qty: 10, LimitPrice: 95, TakeProfitLimit: 98, StopLossStop: 93}, "TP between entry and market is valid for a limit bracket")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", OrderClass: "bracket", TimeInForce: "gtc", Qty: 10, LimitPrice: 95, TakeProfitLimit: 94, StopLossStop: 93}, "TP below the entry price")
	mustReject(t, s, alpaca.OrderRequest{Symbol: "AAA", Side: "buy", Type: "limit", OrderClass: "bracket", TimeInForce: "gtc", Qty: 10, LimitPrice: 95, TakeProfitLimit: 98, StopLossStop: 96}, "SL above the entry price")
}
