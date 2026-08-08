package main

import (
	"bytes"
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

func (l *LLM) Chat(messages []Msg, tools []ToolDef) (*Msg, error) {
	body, err := json.Marshal(chatRequest{Model: l.model, Messages: messages, Tools: tools, MaxTok: 4096})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", l.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.key)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
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
