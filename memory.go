package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Persistence, MimiClaw-style but on a 128 GB microSD instead of 16 MB
// flash: per-channel conversation history (JSON files) and one shared
// MEMORY.md of durable notes, loaded into every system prompt.

var memMu sync.Mutex

func memoryPath(cfg *Config) string { return filepath.Join(cfg.DataDir, "MEMORY.md") }

func readMemory(cfg *Config) string {
	memMu.Lock()
	b, err := os.ReadFile(memoryPath(cfg))
	memMu.Unlock()
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 4000 { // cap what rides the prompt; the full file stays on SD
		n := len(s) - 4000
		for n < len(s) && !utf8.RuneStart(s[n]) { // don't start mid-rune
			n++
		}
		s = "…" + s[n:]
	}
	return s
}

func appendMemory(cfg *Config, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return "memory error: empty note"
	}
	memMu.Lock()
	defer memMu.Unlock()
	f, err := os.OpenFile(memoryPath(cfg), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "memory error: " + err.Error()
	}
	defer f.Close()
	fmt.Fprintf(f, "- [%s] %s\n", time.Now().UTC().Format("2006-01-02"), note)
	return "remembered"
}

// fullMemory returns the whole MEMORY.md (for an admin to inspect for poisoning).
func fullMemory(cfg *Config) string {
	memMu.Lock()
	defer memMu.Unlock()
	b, _ := os.ReadFile(memoryPath(cfg))
	return string(b)
}

// clearMemory wipes long-term memory (admin action).
func clearMemory(cfg *Config) error {
	memMu.Lock()
	defer memMu.Unlock()
	err := os.Remove(memoryPath(cfg))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ── per-channel history ────────────────────────────────────────────────

type History struct {
	mu   sync.Mutex
	cfg  *Config
	byCh map[string][]Msg
}

func NewHistory(cfg *Config) *History {
	return &History{cfg: cfg, byCh: map[string][]Msg{}}
}

func (h *History) path(ch string) string {
	return filepath.Join(h.cfg.DataDir, "history", ch+".json")
}

func (h *History) Get(ch string) []Msg {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.byCh[ch]; ok {
		return append([]Msg(nil), m...)
	}
	var m []Msg
	if b, err := os.ReadFile(h.path(ch)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	h.byCh[ch] = m
	return append([]Msg(nil), m...)
}

// Append stores a user/assistant exchange (tool traffic is not persisted —
// it can be huge and the model re-derives it cheaply).
func (h *History) Append(ch string, exchange ...Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := append(h.byCh[ch], exchange...)
	if max := h.cfg.HistoryTurns * 2; len(m) > max {
		m = m[len(m)-max:]
	}
	h.byCh[ch] = m
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(h.path(ch), b, 0o600)
	}
}
