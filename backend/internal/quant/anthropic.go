package quant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrDisabled means no ANTHROPIC_API_KEY is configured (the agents stay idle / zero-cost).
var ErrDisabled = errors.New("anthropic: disabled (no ANTHROPIC_API_KEY)")

// Anthropic is a generic raw-HTTP Messages API client shared by every Claude agent (Agent 2
// entry on Opus, Agent 3 exit on Haiku, the daily review on Opus). It forces a single structured
// tool call so replies are always clean JSON, caches the system prompt, and reports token usage.
type Anthropic struct {
	apiKey string
	http   *http.Client
}

func NewAnthropic(apiKey string) *Anthropic {
	return &Anthropic{apiKey: strings.TrimSpace(apiKey), http: &http.Client{Timeout: 40 * time.Second}}
}

func (c *Anthropic) Enabled() bool { return c != nil && c.apiKey != "" }

type anthReq struct {
	Model      string         `json:"model"`
	MaxTokens  int            `json:"max_tokens"`
	System     []anthSysBlock `json:"system"`
	Tools      []anthTool     `json:"tools"`
	ToolChoice anthToolChoice `json:"tool_choice"`
	Messages   []anthMsg      `json:"messages"`
}
type anthSysBlock struct {
	Type         string     `json:"type"`
	Text         string     `json:"text"`
	CacheControl *anthCache `json:"cache_control,omitempty"`
}
type anthCache struct {
	Type string `json:"type"`
}
type anthTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}
type anthToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}
type anthMsg struct {
	Role    string           `json:"role"`
	Content []anthMsgContent `json:"content"`
}
type anthMsgContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type anthResp struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Call sends a cached system prompt + a user payload and forces the given tool, returning the
// tool input (raw JSON to be unmarshalled into the agent's decision struct) and token usage.
// Retries transient 429/5xx a few times.
func (c *Anthropic) Call(ctx context.Context, model, system, toolName, toolDesc string, schema map[string]interface{}, userText string, maxTokens int) (json.RawMessage, TokenUsage, error) {
	if !c.Enabled() {
		return nil, TokenUsage{}, ErrDisabled
	}
	if maxTokens <= 0 {
		maxTokens = 512
	}
	reqBody := anthReq{
		Model:      model,
		MaxTokens:  maxTokens,
		System:     []anthSysBlock{{Type: "text", Text: system, CacheControl: &anthCache{Type: "ephemeral"}}},
		Tools:      []anthTool{{Name: toolName, Description: toolDesc, InputSchema: schema}},
		ToolChoice: anthToolChoice{Type: "tool", Name: toolName},
		Messages:   []anthMsg{{Role: "user", Content: []anthMsgContent{{Type: "text", Text: userText}}}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, TokenUsage{}, err
	}

	var body []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(payload))
		if err != nil {
			return nil, TokenUsage{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("anthropic %s", resp.Status)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, TokenUsage{}, fmt.Errorf("anthropic %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		body, lastErr = b, nil
		break
	}
	if body == nil {
		if lastErr == nil {
			lastErr = errors.New("anthropic: no response")
		}
		return nil, TokenUsage{}, lastErr
	}

	var ar anthResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, TokenUsage{}, err
	}
	usage := TokenUsage{
		Input: ar.Usage.InputTokens, Output: ar.Usage.OutputTokens,
		CacheRead: ar.Usage.CacheReadInputTokens, CacheCreate: ar.Usage.CacheCreationInputTokens,
	}
	for _, blk := range ar.Content {
		if blk.Type == "tool_use" && blk.Name == toolName {
			return blk.Input, usage, nil
		}
	}
	return nil, usage, errors.New("anthropic: no tool_use in response")
}
