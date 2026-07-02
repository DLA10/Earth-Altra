// Package gemini is a tiny REST client for Google's Gemini API, used to write a
// plain-language "why is this stock moving?" summary from a news headline/article.
//
// It is deliberately self-throttling: callers hit it ONLY on demand (when the user
// opens a stock's news dropdown), but the client still enforces a per-minute interval
// and a per-day cap so we can never blow through the free-tier limits. When no API key
// is configured the client is disabled and Summarize returns ErrDisabled.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	// ErrDisabled means no API key is configured.
	ErrDisabled = errors.New("gemini: disabled (no API key)")
	// ErrBudget means today's self-imposed daily cap is reached.
	ErrBudget = errors.New("gemini: daily budget reached")
	// ErrBusy means we'd exceed the per-minute ceiling right now; try again shortly.
	ErrBusy = errors.New("gemini: rate ceiling, try again shortly")
	// ErrTransient means a retryable server-side failure (5xx / network) — not the key.
	ErrTransient = errors.New("gemini: transient server error")
)

// Client calls the Gemini generateContent endpoint with a short timeout.
type Client struct {
	apiKey string
	model  string
	http   *http.Client

	mu       sync.Mutex
	minIntvl time.Duration // minimum spacing between calls (from RPM)
	nextAt   time.Time     // earliest time the next call may go out
	day      string        // current UTC day key for the daily counter
	dayCount int
	dailyCap int
}

// New builds a Client. rpm <= 0 disables per-minute spacing; dailyCap <= 0 disables the
// daily cap. An empty apiKey yields a disabled client.
func New(apiKey, model string, rpm, dailyCap int) *Client {
	if strings.TrimSpace(model) == "" {
		model = "gemini-3.5-flash"
	}
	var iv time.Duration
	if rpm > 0 {
		iv = time.Minute / time.Duration(rpm)
	}
	return &Client{
		apiKey:   strings.TrimSpace(apiKey),
		model:    model,
		http:     &http.Client{Timeout: 25 * time.Second},
		minIntvl: iv,
		dailyCap: dailyCap,
	}
}

// Enabled reports whether an API key is configured.
func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

// reserve grants permission for one call now, or returns ErrBudget/ErrBusy. It updates
// the daily counter and the next-allowed time under lock (non-blocking — never sleeps).
func (c *Client) reserve(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	if day != c.day {
		c.day = day
		c.dayCount = 0
	}
	if c.dailyCap > 0 && c.dayCount >= c.dailyCap {
		return ErrBudget
	}
	if c.minIntvl > 0 && now.Before(c.nextAt) {
		return ErrBusy
	}
	c.dayCount++
	c.nextAt = now.Add(c.minIntvl)
	return nil
}

// Budget returns (usedToday, dailyCap) for observability.
func (c *Client) Budget() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dayCount, c.dailyCap
}

// generateContent request/response (only the fields we use).
type genReq struct {
	Contents         []genContent  `json:"contents"`
	Tools            []genTool     `json:"tools,omitempty"`
	GenerationConfig *genGenConfig `json:"generationConfig,omitempty"`
}
type genContent struct {
	Parts []genPart `json:"parts"`
}
type genPart struct {
	Text string `json:"text"`
}
type genTool struct {
	URLContext   *struct{} `json:"url_context,omitempty"`
	GoogleSearch *struct{} `json:"google_search,omitempty"`
}
type genThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}
type genGenConfig struct {
	Temperature     float64            `json:"temperature"`
	MaxOutputTokens int                `json:"maxOutputTokens"`
	ThinkingConfig  *genThinkingConfig `json:"thinkingConfig,omitempty"`
}
type genResp struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// Summarize returns a 1–2 sentence plain-English explanation of the catalyst driving
// `symbol` right now, reasoning over the supplied recent `headlines` (newest first) so it
// names the real current driver rather than an older item. When grounded is true it also
// enables Google Search (more current, but heavily rate-limited on the free tier — use
// sparingly). Honors the daily/minute budgets (returns ErrBudget/ErrBusy without calling).
func (c *Client) Summarize(ctx context.Context, symbol string, headlines []string, grounded bool) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}
	if err := c.reserve(time.Now()); err != nil {
		return "", err
	}

	today := time.Now().Format("January 2, 2006")
	var b strings.Builder
	fmt.Fprintf(&b, "You are a financial news assistant. Today is %s. Based on these recent headlines for %s (newest first), explain in 1-2 plain-English sentences the SPECIFIC catalyst driving the stock's move right now — the actual reason it is rising or falling. ", today, symbol)
	b.WriteString("Pick the dominant, most recent driver (a deal, product, partnership, earnings, guidance, FDA, M&A, analyst action, lawsuit, etc.); ignore generic multi-stock roundups. No investment advice, no preamble.\n\nRecent headlines:\n")
	for i, h := range headlines {
		fmt.Fprintf(&b, "%d. %s\n", i+1, h)
	}

	reqBody := genReq{
		Contents: []genContent{{Parts: []genPart{{Text: b.String()}}}},
	}
	if grounded {
		// Live web search. Allow the model to reason over the search results (no
		// thinkingBudget cap) with a generous output ceiling so it isn't starved.
		reqBody.Tools = []genTool{{GoogleSearch: &struct{}{}}}
		reqBody.GenerationConfig = &genGenConfig{Temperature: 0.2, MaxOutputTokens: 1500}
	} else {
		// Fast text-only path: thinkingBudget 0 (no reasoning tokens) keeps it quick.
		reqBody.GenerationConfig = &genGenConfig{
			Temperature:     0.2,
			MaxOutputTokens: 512,
			ThinkingConfig:  &genThinkingConfig{ThinkingBudget: 0},
		}
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", c.model)

	// One reserved budget unit, but retry the HTTP call a few times on transport errors
	// and 5xx (Gemini returns intermittent 503s under load).
	var body []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 429 (rate limit) and 5xx are transient — back off and retry.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("gemini %s", resp.Status)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("gemini %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		body, lastErr = b, nil
		break
	}
	if body == nil {
		if lastErr == nil {
			lastErr = errors.New("gemini: no response")
		}
		return "", fmt.Errorf("%w: %v", ErrTransient, lastErr)
	}
	var gr genResp
	if err := json.Unmarshal(body, &gr); err != nil {
		return "", err
	}
	if gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("gemini blocked: %s", gr.PromptFeedback.BlockReason)
	}
	var out strings.Builder
	for _, cand := range gr.Candidates {
		for _, p := range cand.Content.Parts {
			out.WriteString(p.Text)
		}
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", errors.New("gemini: empty response")
	}
	return text, nil
}
