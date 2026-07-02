package api

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"live-optimus/backend/internal/alpaca"
)

// NewsHeadline is one article attached to a mover/stock, from any news source.
type NewsHeadline struct {
	Source    string    `json:"source"` // "Benzinga"
	Headline  string    `json:"headline"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	Sentiment string    `json:"sentiment"` // positive | negative | neutral | ""
}

// MoverNews is one market mover plus a lightweight, Alpaca-only news read used to badge
// the row. This is the falling-knife filter the dip-buy strategy needs: a faller with no
// fresh news is a candidate dip; a faller with negative news is a knife to avoid. The
// heavier Gemini summary is fetched separately, on click (see stocknews.go).
type MoverNews struct {
	Symbol        string  `json:"symbol"`
	Price         float64 `json:"price"`
	Change        float64 `json:"change"`
	PercentChange float64 `json:"percent_change"`
	Direction     string  `json:"direction"`    // "gainer" | "loser"
	HasCatalyst   bool    `json:"has_catalyst"` // a fresh (<72h) headline exists
	Sentiment     string  `json:"sentiment"`    // positive | negative | neutral | none
	Why           string  `json:"why"`          // newest headline text
}

// MoversNews is the badge payload returned by GET /api/movers-news. Alpaca-only and
// cheap, so it is safe to refresh on a timer for the whole visible board.
type MoversNews struct {
	Gainers []MoverNews `json:"gainers"`
	Losers  []MoverNews `json:"losers"`
	AsOf    time.Time   `json:"as_of"`
}

// moversNews returns the top gainers/losers each tagged with an Alpaca-only sentiment +
// "has fresh catalyst" flag (drives the DIP?/KNIFE badges). Read-only; never touches the
// order path. ?top=N enriches N per side (default 12, max 25). Cached 45s.
func (s *Server) moversNews(w http.ResponseWriter, r *http.Request) {
	top := 12
	if v := r.URL.Query().Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 25 {
			top = n
		}
	}
	key := strconv.Itoa(top)

	s.mnMu.Lock()
	if s.mnResp != nil && s.mnKey == key && time.Since(s.mnAt) < 45*time.Second {
		resp := *s.mnResp
		s.mnMu.Unlock()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	s.mnMu.Unlock()

	movers, err := s.Client.GetMarketMovers(50)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	gainers := capMovers(movers.Gainers, top)
	losers := capMovers(movers.Losers, top)

	wanted := map[string]bool{}
	var symbols []string
	for _, m := range append(append([]alpaca.MarketMover{}, gainers...), losers...) {
		su := strings.ToUpper(m.Symbol)
		if !wanted[su] {
			wanted[su] = true
			symbols = append(symbols, su)
		}
	}

	// One batched Alpaca (Benzinga) news call covering all the symbols, grouped per name.
	alpBySym := map[string][]alpaca.NewsItem{}
	if len(symbols) > 0 {
		if items, nerr := s.Client.GetNews(symbols, 50); nerr != nil {
			log.Printf("movers-news: alpaca news error: %v", nerr)
		} else {
			for _, n := range items {
				for _, sym := range n.Symbols {
					su := strings.ToUpper(sym)
					if wanted[su] {
						alpBySym[su] = append(alpBySym[su], n)
					}
				}
			}
		}
	}

	resp := &MoversNews{
		Gainers: make([]MoverNews, 0, len(gainers)),
		Losers:  make([]MoverNews, 0, len(losers)),
		AsOf:    time.Now(),
	}
	for _, m := range gainers {
		resp.Gainers = append(resp.Gainers, badgeMover(m, "gainer", alpBySym[strings.ToUpper(m.Symbol)]))
	}
	for _, m := range losers {
		resp.Losers = append(resp.Losers, badgeMover(m, "loser", alpBySym[strings.ToUpper(m.Symbol)]))
	}

	s.mnMu.Lock()
	s.mnResp = resp
	s.mnKey = key
	s.mnAt = time.Now()
	s.mnMu.Unlock()

	writeJSON(w, http.StatusOK, *resp)
}

func capMovers(m []alpaca.MarketMover, n int) []alpaca.MarketMover {
	if len(m) > n {
		return m[:n]
	}
	return m
}

// badgeMover reduces one mover's Alpaca headlines to the badge fields.
func badgeMover(m alpaca.MarketMover, dir string, alp []alpaca.NewsItem) MoverNews {
	// Newest first.
	sort.Slice(alp, func(i, j int) bool { return alp[i].CreatedAt.After(alp[j].CreatedAt) })
	out := MoverNews{
		Symbol:        strings.ToUpper(m.Symbol),
		Price:         m.Price,
		Change:        m.Change,
		PercentChange: m.PercentChange,
		Direction:     dir,
		Sentiment:     aggregateAlpacaSentiment(alp),
	}
	if len(alp) > 0 {
		newest := alp[0]
		out.Why = newest.Headline
		if !newest.CreatedAt.IsZero() && time.Since(newest.CreatedAt) < 72*time.Hour {
			out.HasCatalyst = true
		}
	}
	return out
}

// aggregateAlpacaSentiment reduces a set of Alpaca headlines to one tag; "none" = no news.
func aggregateAlpacaSentiment(items []alpaca.NewsItem) string {
	pos, neg := 0, 0
	for _, n := range items {
		switch n.Sentiment {
		case "positive":
			pos++
		case "negative":
			neg++
		}
	}
	switch {
	case len(items) == 0:
		return "none"
	case pos > neg:
		return "positive"
	case neg > pos:
		return "negative"
	default:
		return "neutral"
	}
}

func normalizeTitle(t string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(t))), " ")
}
