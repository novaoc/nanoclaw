package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 1 && n <= 5 {
		return n
	}
	return def
}

type Config struct {
	DiscordToken  string
	DeepseekKey   string
	DeepseekURL   string // OpenAI-compatible base, default api.deepseek.com
	Model         string
	DataDir       string
	FocusChannels map[string]bool // channel IDs answered without a mention
	MaxToolIters  int
	HistoryTurns  int
	DiveToolIters int // /dive gets a bigger tool budget…
	DivePasses    int // …and N self-review passes (the looper-model play)

	BankrURL string // BANKR_API_URL, default https://api.bankr.bot
	Secret   string // NANOCLAW_SECRET — encrypts users' connected Bankr keys at rest
}

// LoadConfig reads /etc/nanoclaw.env then ./nanoclaw.env (later wins),
// then real environment variables (highest). Simple KEY=VALUE lines.
func LoadConfig() (*Config, error) {
	env := map[string]string{}
	for _, p := range []string{"/etc/nanoclaw.env", "nanoclaw.env"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		f.Close()
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if v := env[k]; v != "" {
			return v
		}
		return def
	}

	cfg := &Config{
		DiscordToken:  get("DISCORD_TOKEN", ""),
		DeepseekKey:   get("DEEPSEEK_API_KEY", ""),
		DeepseekURL:   get("DEEPSEEK_API_URL", "https://api.deepseek.com"),
		Model:         get("NANOCLAW_MODEL", "deepseek-chat"),
		DataDir:       get("NANOCLAW_DATA", "data"),
		FocusChannels: map[string]bool{},
		MaxToolIters:  8,
		HistoryTurns:  24,
		DiveToolIters: 16,
		DivePasses:    atoiOr(get("NANOCLAW_DIVE_PASSES", ""), 2),
		BankrURL:      get("BANKR_API_URL", "https://api.bankr.bot"),
		Secret:        get("NANOCLAW_SECRET", ""),
	}
	for _, id := range strings.Split(get("FOCUS_CHANNELS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.FocusChannels[id] = true
		}
	}
	if cfg.DiscordToken == "" {
		return nil, errors.New("DISCORD_TOKEN is required (nanoclaw.env or environment)")
	}
	if cfg.DeepseekKey == "" {
		return nil, errors.New("DEEPSEEK_API_KEY is required (nanoclaw.env or environment)")
	}
	return cfg, nil
}
