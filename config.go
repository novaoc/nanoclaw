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

// VisionEnabled reports whether Vela can read images sent to Discord.
func (c *Config) VisionEnabled() bool { return c.VisionModel != "" }

// GithubEnabled reports whether the API-only github tool is available (needs a
// token). It's separate from the coder shell: no box access, just GitHub API.
func (c *Config) GithubEnabled() bool { return c.GitHubToken != "" }

// RepoAllowed gates the github tool. Empty RepoUsers = open to everyone in the
// server (the private-server default); a non-empty list restricts to those IDs.
func (c *Config) RepoAllowed(uid string) bool {
	return len(c.RepoUsers) == 0 || c.RepoUsers[uid]
}

// CodeEnabled reports whether the shell/file tools may run — gated to an
// explicit coder allowlist (NANOCLAW_CODERS). A shell is root-level trust on
// the box, so an empty allowlist turns the whole capability off.
func (c *Config) CodeEnabled() bool {
	return len(c.Coders) > 0
}

type Config struct {
	DiscordToken  string
	DeepseekKey   string
	DeepseekURL   string // OpenAI-compatible base, default api.deepseek.com
	Model         string
	VisionModel   string // NANOCLAW_VISION_MODEL — reads images sent to Discord; "" disables
	DataDir       string
	FocusChannels map[string]bool // channel IDs answered without a mention
	MaxToolIters  int
	HistoryTurns  int
	DiveToolIters int // /dive gets a bigger tool budget…
	DivePasses    int // …and N self-review passes (the looper-model play)
	BraveKey      string // BRAVE_API_KEY — real search API; falls back to DuckDuckGo scraping

	Coders      map[string]bool // Discord IDs allowed to run shell/code (root-trust)
	RepoUsers   map[string]bool // Discord IDs allowed the github API tool; empty = everyone
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
		VisionModel:   get("NANOCLAW_VISION_MODEL", "stepfun/step-3.7-flash"),
		DataDir:       get("NANOCLAW_DATA", "data"),
		FocusChannels: map[string]bool{},
		MaxToolIters:  8,
		HistoryTurns:  24,
		DiveToolIters: 16,
		DivePasses:    atoiOr(get("NANOCLAW_DIVE_PASSES", ""), 2),
		BraveKey:      get("BRAVE_API_KEY", ""),
		Coders:        map[string]bool{},
		RepoUsers:     map[string]bool{},
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
	for _, id := range strings.Split(get("NANOCLAW_REPO_USERS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.RepoUsers[id] = true
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
