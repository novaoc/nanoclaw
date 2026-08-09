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

// CodeEnabled reports whether the shell/file tools may run. It enforces the
// custody/execution split: code is NOT available while THIS process holds
// wallet-key material (NANOCLAW_SECRET), because a shell in the same process
// could read the secret and every connected user's keys and exfiltrate them.
// To have both a coder shell AND wallets, move key custody into clawvault (a
// separate process/user) so this one never holds the secret. See CLAWVAULT.md.
func (c *Config) CodeEnabled() bool {
	return len(c.Coders) > 0 && c.Secret == ""
}

// CodeInterlockTripped is true when a misconfig asks for BOTH code and
// in-process keys — code is refused and startup logs why.
func (c *Config) CodeInterlockTripped() bool {
	return len(c.Coders) > 0 && c.Secret != ""
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

	Coders      map[string]bool // Discord IDs allowed to run shell/code (root-trust)
	Workspace   string          // where code lives + shell runs
	GitHubToken string          // GITHUB_TOKEN — enables authenticated push
	GitName     string          // commit identity (Vela's own account)
	GitEmail    string
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
		Coders:        map[string]bool{},
		Workspace:     get("NANOCLAW_WORKSPACE", ""),
		GitHubToken:   get("GITHUB_TOKEN", ""),
		GitName:       get("GIT_NAME", "Vela"),
		GitEmail:      get("GIT_EMAIL", ""),
	}
	if cfg.Workspace == "" {
		cfg.Workspace = cfg.DataDir + "/workspace"
	}
	for _, id := range strings.Split(get("FOCUS_CHANNELS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.FocusChannels[id] = true
		}
	}
	for _, id := range strings.Split(get("NANOCLAW_CODERS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.Coders[id] = true
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
