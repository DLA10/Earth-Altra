package alpaca

import (
	"strings"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// NewsItem is a JSON-friendly news article with a lightweight sentiment tag.
type NewsItem struct {
	ID        int       `json:"id"`
	Headline  string    `json:"headline"`
	Summary   string    `json:"summary"`
	Author    string    `json:"author"`
	URL       string    `json:"url"`
	Symbols   []string  `json:"symbols"`
	CreatedAt time.Time `json:"created_at"`
	Sentiment string    `json:"sentiment"` // positive | negative | neutral
}

// GetNews returns recent Benzinga headlines for the given symbols (most recent first).
func (c *Client) GetNews(symbols []string, limit int) ([]NewsItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	news, err := c.data.GetNews(marketdata.GetNewsRequest{
		Symbols:    symbols,
		Sort:       marketdata.SortDesc,
		TotalLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]NewsItem, 0, len(news))
	for _, n := range news {
		out = append(out, NewsItem{
			ID:        n.ID,
			Headline:  n.Headline,
			Summary:   n.Summary,
			Author:    n.Author,
			URL:       n.URL,
			Symbols:   n.Symbols,
			CreatedAt: n.CreatedAt,
			Sentiment: classifySentiment(n.Headline + " " + n.Summary),
		})
	}
	return out, nil
}

// Lightweight keyword sentiment. Not a model — a quick at-a-glance tag the user can
// override with their own read of the headline. Easy to upgrade to a real classifier.
var (
	posWords = []string{"surge", "soar", "beat", "beats", "rally", "jump", "gain", "upgrade", "record", "strong", "tops", "boost", "rise", "win", "approval", "outperform", "raise", "bullish", "growth", "profit", "soars", "surges", "rallies", "jumps"}
	negWords = []string{"plunge", "fall", "drop", "miss", "downgrade", "cut", "weak", "loss", "lawsuit", "probe", "decline", "slump", "warning", "halt", "bearish", "slash", "sink", "tumble", "recall", "investigation", "plunges", "falls", "drops", "misses", "cuts", "fraud", "bankrupt"}
)

func classifySentiment(text string) string {
	t := strings.ToLower(text)
	pos, neg := 0, 0
	for _, w := range posWords {
		if strings.Contains(t, w) {
			pos++
		}
	}
	for _, w := range negWords {
		if strings.Contains(t, w) {
			neg++
		}
	}
	if pos > neg {
		return "positive"
	}
	if neg > pos {
		return "negative"
	}
	return "neutral"
}
