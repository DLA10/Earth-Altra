package alpaca

import (
	"context"
	"fmt"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata/stream"

	"live-optimus/backend/internal/candles"
)

// StreamHandlers receives normalized market-data events.
type StreamHandlers struct {
	OnTrade func(symbol string, t time.Time, price, size float64)
	OnBar   func(symbol string, t time.Time, open, high, low, clse, volume, vwap float64)
	OnQuote func(symbol string, bid, ask float64, t time.Time)
}

// HistBar is a neutral historical bar (decoupled from SDK types).
type HistBar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	VWAP   float64
}

// Backfill loads 1-minute bars for a symbol from the start of the most recent
// trading day through now, so the chart shows the full session with no gaps — even
// if the app was closed/asleep and reopened mid-session. Alpaca stores every minute
// bar regardless of whether we were connected, so this heals any downtime gap.
// TotalLimit is left unset (0 = all bars in the window); the window is bounded to one
// day so the count is naturally capped.
func (c *Client) Backfill(symbol string) ([]candles.Candle, error) {
	end := time.Now().Add(-1 * time.Minute) // SIP has no 15-min delay, but skip the partial latest minute
	start := sessionDayStartET(end)
	bars, err := c.data.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame: marketdata.OneMin,
		Start:     start,
		End:       end,
		Feed:      c.feed,
	})
	if err != nil {
		return nil, err
	}
	out := make([]candles.Candle, 0, len(bars))
	for _, b := range bars {
		out = append(out, candles.Candle{
			Time:   b.Timestamp.Unix(),
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: float64(b.Volume),
		})
	}
	return out, nil
}

// sessionDayStartET returns 00:00 ET of the most recent US trading day on/before t,
// so backfill captures the whole day (premarket + regular + after-hours) up to now.
// Mirrors the scanner's session logic: walks back over the pre-open window and
// weekends so the returned day always has a real session.
func sessionDayStartET(t time.Time) time.Time {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	n := t.In(et)
	start := time.Date(n.Year(), n.Month(), n.Day(), 9, 30, 0, 0, et)
	if n.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	for start.Weekday() == time.Saturday || start.Weekday() == time.Sunday {
		start = start.AddDate(0, 0, -1)
	}
	return time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, et)
}

// StartStream connects to the single Alpaca SIP data connection (Alpaca permits only
// one per account) and subscribes trades+quotes for tqSymbols (the execution engine's
// symbols) and bars for barSymbols (the union of execution + scanner universe). It
// blocks until ctx is cancelled or the stream terminates.
func (c *Client) StartStream(ctx context.Context, tqSymbols, barSymbols []string, h StreamHandlers) error {
	// Build SDK-level handlers once; retain trade/quote handlers so single symbols can
	// be subscribed at runtime (e.g. when the user adds one from DECEPTICON).
	tradeCb := func(t stream.Trade) {
		h.OnTrade(t.Symbol, t.Timestamp, t.Price, float64(t.Size))
	}
	quoteCb := func(q stream.Quote) {
		h.OnQuote(q.Symbol, q.BidPrice, q.AskPrice, q.Timestamp)
	}
	barCb := func(b stream.Bar) {
		h.OnBar(b.Symbol, b.Timestamp, b.Open, b.High, b.Low, b.Close, float64(b.Volume), b.VWAP)
	}

	opts := []stream.StockOption{stream.WithCredentials(c.cfg.APIKeyID, c.cfg.APISecret)}
	if h.OnTrade != nil && len(tqSymbols) > 0 {
		opts = append(opts, stream.WithTrades(tradeCb, tqSymbols...))
	}
	if h.OnQuote != nil && len(tqSymbols) > 0 {
		opts = append(opts, stream.WithQuotes(quoteCb, tqSymbols...))
	}
	if h.OnBar != nil && len(barSymbols) > 0 {
		opts = append(opts, stream.WithBars(barCb, barSymbols...))
	}

	sc := stream.NewStocksClient(c.feed, opts...)
	if err := sc.Connect(ctx); err != nil {
		return fmt.Errorf("stream connect: %w", err)
	}

	c.streamMu.Lock()
	c.stockStream = sc
	c.tradeCb = tradeCb
	c.quoteCb = quoteCb
	c.streamMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-sc.Terminated():
		c.streamMu.Lock()
		if c.stockStream == sc {
			c.stockStream = nil
		}
		c.streamMu.Unlock()
		return err
	}
}

// SubscribeTradeQuote adds live trade+quote subscriptions for a symbol on the current
// connection (bars for DECEPTICON symbols are already subscribed). No-op if the stream
// isn't connected yet — runStream re-subscribes the full set on each (re)connect.
func (c *Client) SubscribeTradeQuote(symbols ...string) error {
	c.streamMu.RLock()
	sc, tcb, qcb := c.stockStream, c.tradeCb, c.quoteCb
	c.streamMu.RUnlock()
	if sc == nil {
		return nil
	}
	if tcb != nil {
		if err := sc.SubscribeToTrades(tcb, symbols...); err != nil {
			return err
		}
	}
	if qcb != nil {
		if err := sc.SubscribeToQuotes(qcb, symbols...); err != nil {
			return err
		}
	}
	return nil
}

// UnsubscribeTradeQuote removes live trade+quote subscriptions for a symbol.
func (c *Client) UnsubscribeTradeQuote(symbols ...string) error {
	c.streamMu.RLock()
	sc := c.stockStream
	c.streamMu.RUnlock()
	if sc == nil {
		return nil
	}
	_ = sc.UnsubscribeFromTrades(symbols...)
	_ = sc.UnsubscribeFromQuotes(symbols...)
	return nil
}

// GetMultiDailyBars fetches recent daily bars for many symbols in batched requests.
func (c *Client) GetMultiDailyBars(symbols []string, days int) (map[string][]HistBar, error) {
	end := time.Now()
	start := end.AddDate(0, 0, -days*2) // calendar buffer for weekends/holidays
	return c.getMulti(symbols, marketdata.OneDay, start, end)
}

// GetMultiIntradayBars fetches 1-minute bars for many symbols between start and end.
func (c *Client) GetMultiIntradayBars(symbols []string, start, end time.Time) (map[string][]HistBar, error) {
	return c.getMulti(symbols, marketdata.OneMin, start, end)
}

// RangeBars fetches split-adjusted historical bars for a UI lookback range — "1W", "1M",
// "6M", or "1Y" (caller passes it already upper-cased). Daily bars for the month-plus
// ranges, hourly for one week; lookback starts include a buffer so weekends/holidays don't
// shorten the visible window. This is a pure REST read, independent of the live candle
// engine, so it serves ANY symbol and never touches the streaming path.
func (c *Client) RangeBars(symbol, rng string) ([]HistBar, error) {
	now := time.Now()
	var tf marketdata.TimeFrame
	var start time.Time
	switch rng {
	case "1W":
		tf, start = marketdata.OneHour, now.AddDate(0, 0, -10)
	case "1M":
		tf, start = marketdata.OneDay, now.AddDate(0, -1, -7)
	case "6M":
		tf, start = marketdata.OneDay, now.AddDate(0, -6, -7)
	case "1Y":
		tf, start = marketdata.OneDay, now.AddDate(-1, 0, -7)
	default:
		return nil, fmt.Errorf("unknown range %q", rng)
	}
	bars, err := c.data.GetBars(symbol, marketdata.GetBarsRequest{
		TimeFrame:  tf,
		Adjustment: marketdata.Split,
		Start:      start,
		End:        now.Add(-1 * time.Minute), // skip the partial latest minute
		Feed:       c.feed,
	})
	if err != nil {
		return nil, err
	}
	out := make([]HistBar, 0, len(bars))
	for _, b := range bars {
		out = append(out, HistBar{
			Time:   b.Timestamp,
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: float64(b.Volume),
			VWAP:   b.VWAP,
		})
	}
	return out, nil
}

func (c *Client) getMulti(symbols []string, tf marketdata.TimeFrame, start, end time.Time) (map[string][]HistBar, error) {
	out := map[string][]HistBar{}
	// Batch to keep individual responses reasonable.
	const batch = 100
	for i := 0; i < len(symbols); i += batch {
		j := i + batch
		if j > len(symbols) {
			j = len(symbols)
		}
		res, err := c.data.GetMultiBars(symbols[i:j], marketdata.GetBarsRequest{
			TimeFrame: tf,
			Start:     start,
			End:       end,
			Feed:      c.feed,
		})
		if err != nil {
			return out, err
		}
		for sym, bars := range res {
			hs := make([]HistBar, 0, len(bars))
			for _, b := range bars {
				hs = append(hs, HistBar{
					Time:   b.Timestamp,
					Open:   b.Open,
					High:   b.High,
					Low:    b.Low,
					Close:  b.Close,
					Volume: float64(b.Volume),
					VWAP:   b.VWAP,
				})
			}
			out[sym] = hs
		}
	}
	return out, nil
}
