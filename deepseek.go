package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Minimal OpenAI-compatible chat client — DeepSeek speaks this dialect,
// including function calling on deepseek-chat.

type Msg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Msg     `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	MaxTok   int       `json:"max_tokens,omitempty"`
}

// Multimodal request shapes (OpenAI vision format): the user message content
// is an array of text/image parts instead of a plain string.
type visionImgURL struct {
	URL string `json:"url"`
}
type visionPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *visionImgURL `json:"image_url,omitempty"`
}
type visionMsg struct {
	Role    string       `json:"role"`
	Content []visionPart `json:"content"`
}
type reasoningCfg struct {
	MaxTokens int `json:"max_tokens,omitempty"`
}
type visionRequest struct {
	Model     string        `json:"model"`
	Messages  []visionMsg   `json:"messages"`
	MaxTok    int           `json:"max_tokens,omitempty"`
	Reasoning *reasoningCfg `json:"reasoning,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Msg `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type LLM struct {
	baseURL string
	key     string
	model   string
	client  *http.Client
}

func NewLLM(cfg *Config) *LLM {
	return &LLM{
		baseURL: cfg.DeepseekURL,
		key:     cfg.DeepseekKey,
		model:   cfg.Model,
		client:  &http.Client{Timeout: 180 * time.Second},
	}
}

func (l *LLM) Chat(ctx context.Context, messages []Msg, tools []ToolDef) (*Msg, error) {
	body, err := json.Marshal(chatRequest{Model: l.model, Messages: messages, Tools: tools, MaxTok: 4096})
	if err != nil {
		return nil, err
	}
	return l.post(ctx, body)
}

// post sends a pre-marshalled chat/completions body and retries transient
// failures. The per-turn ctx bounds total time so a hung upstream can't hold
// the turn (and its concurrency slot) indefinitely. Shared by Chat and Vision.
func (l *LLM) post(ctx context.Context, body []byte) (*Msg, error) {
	// Inference providers 429/503 under load routinely — retry with backoff
	// instead of surfacing a transient blip to the channel.
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 2 * time.Second) // 2s, 8s, 18s
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("turn deadline reached: %w", ctx.Err())
		}
		req, err := http.NewRequestWithContext(ctx, "POST", l.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+l.key)
		resp, err := l.client.Do(req)
		if err != nil {
			lastErr = err
			continue // network blip
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("provider %d: %.200s", resp.StatusCode, raw)
			continue
		}
		var out chatResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("bad response (%d): %.200s", resp.StatusCode, raw)
		}
		if out.Error != nil {
			return nil, fmt.Errorf("api: %s", out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return nil, fmt.Errorf("empty response (%d)", resp.StatusCode)
		}
		return &out.Choices[0].Message, nil
	}
	return nil, fmt.Errorf("provider unavailable after retries: %w", lastErr)
}

// Vision runs one multimodal turn on the vision model and returns its text.
// Images are data-URI or https strings in OpenAI's image_url format. The vision
// model is a reasoner that can think past the token budget and return EMPTY
// content (finish_reason "length"); we cap its thinking so the answer always
// lands, and still give the overall response generous room.
func (l *LLM) Vision(ctx context.Context, model, prompt string, imageURLs []string) (string, error) {
	parts := []visionPart{{Type: "text", Text: prompt}}
	for _, u := range imageURLs {
		parts = append(parts, visionPart{Type: "image_url", ImageURL: &visionImgURL{URL: u}})
	}
	body, err := json.Marshal(visionRequest{
		Model:     model,
		Messages:  []visionMsg{{Role: "user", Content: parts}},
		MaxTok:    6000,
		Reasoning: &reasoningCfg{MaxTokens: 2000},
	})
	if err != nil {
		return "", err
	}
	msg, err := l.post(ctx, body)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}
