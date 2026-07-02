package quant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Agent4 is the SENTIMENT agent. It runs on the user's LOCAL LLM (Ollama / gemma2:2b) — free and
// fast — because sentiment classification is a simple task that doesn't need reasoning horsepower.
// It is advisory ONLY: it scores each universe symbol's recent headlines on a slow loop and caches
// the result; Agent 2 reads the cache at decision time. It never decides a trade.
type Agent4 struct {
	endpoint string // e.g. http://localhost:11434
	model    string // e.g. gemma2:2b
	http     *http.Client
	newsFn   func(sym string, limit int) []string
	loc      *time.Location

	mu    sync.RWMutex
	cache map[string]SentimentScore
}

func NewAgent4(endpoint, model string, newsFn func(sym string, limit int) []string, loc *time.Location) *Agent4 {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "http://localhost:11434"
	}
	if strings.TrimSpace(model) == "" {
		model = "gemma2:2b"
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Agent4{
		endpoint: strings.TrimRight(endpoint, "/"), model: model, newsFn: newsFn, loc: loc,
		http: &http.Client{Timeout: 30 * time.Second}, cache: map[string]SentimentScore{},
	}
}

// Enabled reports whether a news source is wired (the local model is assumed reachable; failures
// are logged and the cache simply stays empty — Agent 2 then treats sentiment as unknown).
func (a *Agent4) Enabled() bool { return a != nil && a.newsFn != nil }

// Get returns the cached sentiment for a symbol (ok=false if none yet).
func (a *Agent4) Get(sym string) (SentimentScore, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.cache[strings.ToUpper(strings.TrimSpace(sym))]
	return s, ok
}

// Run refreshes sentiment for the current universe symbols every 5 minutes until ctx is done.
func (a *Agent4) Run(ctx context.Context, symbolsFn func() []string) {
	if !a.Enabled() {
		return
	}
	a.refresh(symbolsFn)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh(symbolsFn)
		}
	}
}

func (a *Agent4) refresh(symbolsFn func() []string) {
	for _, sym := range symbolsFn() {
		heads := a.newsFn(sym, 6)
		if len(heads) == 0 {
			continue
		}
		s, err := a.score(sym, heads)
		if err != nil {
			log.Printf("[agent4] %s sentiment failed: %v", sym, err)
			continue
		}
		a.mu.Lock()
		a.cache[strings.ToUpper(sym)] = s
		a.mu.Unlock()
	}
}

type ollamaReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	Format string `json:"format"`
}
type ollamaResp struct {
	Response string `json:"response"`
}

// score asks the local model to classify the headlines' short-term trading sentiment.
func (a *Agent4) score(sym string, headlines []string) (SentimentScore, error) {
	var sb strings.Builder
	sb.WriteString("You are a financial news sentiment classifier for short-term stock trading. ")
	sb.WriteString("Given recent headlines for ")
	sb.WriteString(sym)
	sb.WriteString(", classify the overall NEXT-FEW-HOURS trading sentiment. Respond ONLY with JSON: ")
	sb.WriteString(`{"lean":"positive|neutral|negative","score":<-1.0..1.0>,"has_catalyst":<true|false>,"why":"<=12 words"}. `)
	sb.WriteString("has_catalyst is true if there is a concrete market-moving event (earnings, guidance, M&A, FDA, analyst action). Headlines:\n")
	for _, h := range headlines {
		sb.WriteString("- ")
		sb.WriteString(h)
		sb.WriteString("\n")
	}

	body, _ := json.Marshal(ollamaReq{Model: a.model, Prompt: sb.String(), Stream: false, Format: "json"})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return SentimentScore{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return SentimentScore{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return SentimentScore{}, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var or ollamaResp
	if err := json.Unmarshal(rb, &or); err != nil {
		return SentimentScore{}, err
	}
	var s SentimentScore
	if err := json.Unmarshal([]byte(or.Response), &s); err != nil {
		return SentimentScore{}, fmt.Errorf("parse sentiment json: %w", err)
	}
	s.Symbol = strings.ToUpper(sym)
	s.Lean = strings.ToLower(strings.TrimSpace(s.Lean))
	if s.Lean != "positive" && s.Lean != "negative" {
		s.Lean = "neutral"
	}
	if s.Score < -1 {
		s.Score = -1
	} else if s.Score > 1 {
		s.Score = 1
	}
	s.UpdatedAt = time.Now().In(a.loc)
	return s, nil
}
