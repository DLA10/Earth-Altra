package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"live-optimus/backend/internal/gemini"
)

// snEntry caches one stock's headlines (the cheap part) plus the top headline lines the
// background summarizer feeds to Gemini.
type snEntry struct {
	data         StockNews
	llmHeadlines []string // top "headline — wire summary" lines, newest first
	at           time.Time
}

// sumEntry caches a background-computed Gemini summary.
type sumEntry struct {
	summary string
	status  string // ok | no_news | disabled | budget | error
	at      time.Time
}

// StockNews is the on-click dropdown payload. Headlines come back immediately; the AI
// summary is filled from a background cache (summary_status "pending" until it lands).
type StockNews struct {
	Symbol        string         `json:"symbol"`
	Summary       string         `json:"summary"`        // Gemini "why", may be ""
	SummaryStatus string         `json:"summary_status"` // pending|ok|no_news|disabled|budget|busy|error
	Sentiment     string         `json:"sentiment"`      // positive|negative|neutral|none
	HasCatalyst   bool           `json:"has_catalyst"`
	Headlines     []NewsHeadline `json:"headlines"`
	AsOf          time.Time      `json:"as_of"`
}

type rawArticle struct {
	source    string
	headline  string
	url       string
	summary   string
	sentiment string
	t         time.Time
}

// stockNews returns one stock's headlines instantly and a background-computed AI summary.
// Read-only; never touches the order path. Query: ?symbol=XYZ (required). Headlines cached
// 30s; the summary is computed off the request path so this handler always returns fast.
func (s *Server) stockNews(w http.ResponseWriter, r *http.Request) {
	sym := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if sym == "" {
		writeErr(w, http.StatusBadRequest, "symbol is required")
		return
	}

	// Headlines: from the short cache, or fetch fresh (Alpaca is cheap).
	var entry snEntry
	s.snMu.Lock()
	cached, ok := s.snCache[sym]
	s.snMu.Unlock()
	if ok && time.Since(cached.at) < 30*time.Second {
		entry = cached
	} else {
		entry = s.buildHeadlines(sym)
		s.snMu.Lock()
		if s.snCache == nil {
			s.snCache = map[string]snEntry{}
		}
		s.snCache[sym] = entry
		s.snMu.Unlock()
	}

	out := entry.data
	out.AsOf = time.Now()
	out.Summary, out.SummaryStatus = s.getOrStartSummary(sym, entry.llmHeadlines)
	writeJSON(w, http.StatusOK, out)
}

// buildHeadlines fetches the headlines for one symbol (Alpaca/Benzinga) and assembles the
// headlines-only StockNews.
func (s *Server) buildHeadlines(sym string) snEntry {
	var raw []rawArticle
	if items, err := s.Client.GetNews([]string{sym}, 8); err != nil {
		log.Printf("stock-news %s: alpaca error: %v", sym, err)
	} else {
		for _, n := range items {
			raw = append(raw, rawArticle{
				source: "Benzinga", headline: n.Headline, url: n.URL,
				summary: n.Summary, sentiment: n.Sentiment, t: n.CreatedAt,
			})
		}
	}

	raw = dedupeArticles(raw)
	sort.Slice(raw, func(i, j int) bool { return raw[i].t.After(raw[j].t) })
	if len(raw) > 8 {
		raw = raw[:8]
	}

	// Headlines is initialized non-nil so it always marshals to [] (not null), else the
	// UI crashes reading .length on a no-news stock.
	out := StockNews{Symbol: sym, Headlines: []NewsHeadline{}}
	pos, neg := 0, 0
	for _, a := range raw {
		out.Headlines = append(out.Headlines, NewsHeadline{
			Source: a.source, Headline: a.headline, URL: a.url, CreatedAt: a.t, Sentiment: a.sentiment,
		})
		switch a.sentiment {
		case "positive":
			pos++
		case "negative":
			neg++
		}
	}
	switch {
	case len(raw) == 0:
		out.Sentiment = "none"
	case pos > neg:
		out.Sentiment = "positive"
	case neg > pos:
		out.Sentiment = "negative"
	default:
		out.Sentiment = "neutral"
	}
	e := snEntry{data: out, at: time.Now()}
	if len(raw) > 0 {
		if !raw[0].t.IsZero() && time.Since(raw[0].t) < 72*time.Hour {
			e.data.HasCatalyst = true
		}
		// Feed Gemini the top headlines (+ trimmed wire summary), newest first, so it can
		// identify the real catalyst rather than whatever the single newest item is (which
		// is often a generic multi-stock roundup).
		n := len(raw)
		if n > 6 {
			n = 6
		}
		for _, a := range raw[:n] {
			line := a.headline
			if a.summary != "" {
				sm := a.summary
				if len(sm) > 280 {
					sm = sm[:280]
				}
				line += " — " + sm
			}
			e.llmHeadlines = append(e.llmHeadlines, line)
		}
	}
	return e
}

// getOrStartSummary returns the cached summary, or "pending" (kicking off a single-flight
// background compute). It never blocks on the model.
func (s *Server) getOrStartSummary(sym string, headlines []string) (string, string) {
	if len(headlines) == 0 {
		return "", "no_news"
	}
	if s.Gemini == nil || !s.Gemini.Enabled() {
		return "", "disabled"
	}
	s.sumMu.Lock()
	defer s.sumMu.Unlock()
	if s.sumCache != nil {
		if e, ok := s.sumCache[sym]; ok {
			ttl := 15 * time.Minute
			if e.status == "error" { // terminal error: re-try after a short while
				ttl = 2 * time.Minute
			}
			if time.Since(e.at) < ttl {
				return e.summary, e.status
			}
		}
	}
	if s.sumInflight == nil {
		s.sumInflight = map[string]bool{}
	}
	if !s.sumInflight[sym] {
		s.sumInflight[sym] = true
		go s.computeSummary(sym, headlines)
	}
	return "", "pending"
}

// computeSummary runs the Gemini call off the request path, waiting out the per-minute
// ceiling rather than failing, and stores the result for the polling UI to pick up. Uses
// a fast text summary (no article fetch) so it returns in ~1-2s once a slot is free.
func (s *Server) computeSummary(sym string, headlines []string) {
	defer func() {
		s.sumMu.Lock()
		delete(s.sumInflight, sym)
		s.sumMu.Unlock()
	}()

	// Non-grounded summary over the top headlines — reliable and fast. (Google Search
	// grounding is heavily rate-limited on the free tier, so it is not used by default.)
	summary, status := s.runSummary(sym, headlines, false)
	// Cache ok/budget/error/disabled so the UI stops polling; leave a transient give-up
	// ("" status) UNcached so the next poll re-spawns and keeps trying.
	if status == "" {
		return
	}
	s.sumMu.Lock()
	if s.sumCache == nil {
		s.sumCache = map[string]sumEntry{}
	}
	s.sumCache[sym] = sumEntry{summary: summary, status: status, at: time.Now()}
	s.sumMu.Unlock()
}

// runSummary runs one Summarize attempt-loop, retrying the per-minute ceiling and Google's
// transient 429/5xx. Returns ("", "") if it exhausts retries while still transient.
func (s *Server) runSummary(sym string, headlines []string, grounded bool) (string, string) {
	for attempt := 0; attempt < 6; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		text, err := s.Gemini.Summarize(ctx, sym, headlines, grounded)
		cancel()
		switch {
		case err == nil:
			return text, "ok"
		case errors.Is(err, gemini.ErrBusy):
			time.Sleep(8 * time.Second)
		case errors.Is(err, gemini.ErrTransient):
			time.Sleep(3 * time.Second)
		case errors.Is(err, gemini.ErrBudget):
			return "", "budget"
		case errors.Is(err, gemini.ErrDisabled):
			return "", "disabled"
		default:
			log.Printf("stock-news %s: gemini error (grounded=%v): %v", sym, grounded, err)
			return "", "error"
		}
	}
	return "", "" // exhausted retries while transient
}

// dedupeArticles drops duplicate headlines (same normalized title), keeping the copy that
// carries a sentiment, else the newer one. Insertion order is preserved.
func dedupeArticles(in []rawArticle) []rawArticle {
	byTitle := map[string]rawArticle{}
	var order []string
	for _, a := range in {
		k := normalizeTitle(a.headline)
		if k == "" {
			continue
		}
		if ex, ok := byTitle[k]; ok {
			if ex.sentiment == "" && a.sentiment != "" {
				byTitle[k] = a
			} else if a.t.After(ex.t) {
				byTitle[k] = a
			}
			continue
		}
		byTitle[k] = a
		order = append(order, k)
	}
	out := make([]rawArticle, 0, len(order))
	for _, k := range order {
		out = append(out, byTitle[k])
	}
	return out
}
