package alpaca

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// MarketMover is one entry in the screener's gainers/losers list.
type MarketMover struct {
	Symbol        string  `json:"symbol"`
	Price         float64 `json:"price"`
	Change        float64 `json:"change"`
	PercentChange float64 `json:"percent_change"`
}

// MarketMovers is the screener's top gainers and losers (market-wide, real-time SIP).
type MarketMovers struct {
	Gainers []MarketMover `json:"gainers"`
	Losers  []MarketMover `json:"losers"`
}

// GetMarketMovers returns the market-wide top gainers and losers (up to `top`, max 50)
// from Alpaca's Screener API. The v3 SDK doesn't wrap this endpoint, so we call it
// directly with the data credentials.
func (c *Client) GetMarketMovers(top int) (*MarketMovers, error) {
	if top <= 0 || top > 50 {
		top = 50
	}
	url := fmt.Sprintf("%s/v1beta1/screener/stocks/movers?top=%d", c.cfg.DataBaseURL, top)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("APCA-API-KEY-ID", c.cfg.APIKeyID)
	req.Header.Set("APCA-API-SECRET-KEY", c.cfg.APISecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("movers request failed (%s): %s", resp.Status, string(b))
	}
	var m MarketMovers
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}
