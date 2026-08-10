package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// focusPath persists runtime /focus channel choices next to the data dir.
func focusPath(dataDir string) string { return filepath.Join(dataDir, "focus-channels.txt") }

func loadFocus(dataDir string) []string {
	b, err := os.ReadFile(focusPath(dataDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func saveFocus(dataDir string, ids map[string]bool) {
	var b strings.Builder
	for id, on := range ids {
		if on {
			b.WriteString(id + "\n")
		}
	}
	_ = os.WriteFile(focusPath(dataDir), []byte(b.String()), 0o644)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 1 && n <= 5 {
		return n
	}
	return def
}

// clampInt parses s and keeps it within [lo,hi], else returns def.
func clampInt(s string, def, lo, hi int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= lo && n <= hi {
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

// XAIEnabled reports whether xAI (Grok) image/video generation is available.
func (c *Config) XAIEnabled() bool { return c.XAIKey != "" }

// ImageAllowed gates the image/video tools. Empty ImageUsers = open to everyone
// in the server (private-server default); a non-empty list restricts to those IDs.
func (c *Config) ImageAllowed(uid string) bool {
	return len(c.ImageUsers) == 0 || c.ImageUsers[uid]
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
	Concurrency   int // max turns running at once; 1 = strict queue (RAM-safe on the Nano)
	HistoryTurns  int
	DiveToolIters int // /dive gets a bigger tool budget…
	DivePasses    int // …and N self-review passes (the looper-model play)
	BraveKey      string // BRAVE_API_KEY — real search API; falls back to DuckDuckGo scraping

	Coders      map[string]bool // Discord IDs allowed to run shell/code (root-trust)
	RepoUsers   map[string]bool // Discord IDs allowed the github API tool; empty = everyone
	Mods        map[string]bool // Discord IDs allowed moderation (NANOCLAW_MODS); empty = off
	ImageUsers  map[string]bool // Discord IDs allowed xAI image/video gen; empty = everyone

	XAIKey        string // XAI_API_KEY (console.x.ai — NOT a SuperGrok sub); "" disables image/video gen
	XAIURL        string // XAI_API_URL, default https://api.x.ai/v1
	XAIImageModel string // XAI_IMAGE_MODEL
	XAIVideoModel string // XAI_VIDEO_MODEL
	Workspace   string          // where code lives + shell runs
	GitHubToken string          // GITHUB_TOKEN — enables authenticated push
	GitName     string          // commit identity (Vela's own account)
	GitEmail    string
	Secrets     *SecretStore // ephemeral deploy-key store (never in prompt/history)
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
		MaxToolIters:  clampInt(get("NANOCLAW_MAX_ITERS", ""), 20, 4, 40),
		Concurrency:   clampInt(get("NANOCLAW_CONCURRENCY", ""), 1, 1, 4),
		HistoryTurns:  24,
		DiveToolIters: 16,
		DivePasses:    atoiOr(get("NANOCLAW_DIVE_PASSES", ""), 2),
		BraveKey:      get("BRAVE_API_KEY", ""),
		Coders:        map[string]bool{},
		RepoUsers:     map[string]bool{},
		Mods:          map[string]bool{},
		ImageUsers:    map[string]bool{},
		XAIKey:        get("XAI_API_KEY", ""),
		XAIURL:        get("XAI_API_URL", "https://api.x.ai/v1"),
		XAIImageModel: get("XAI_IMAGE_MODEL", "grok-imagine-image-quality"),
		XAIVideoModel: get("XAI_VIDEO_MODEL", "grok-imagine-video-1.5"),
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
	for _, id := range strings.Split(get("NANOCLAW_MODS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.Mods[id] = true
		}
	}
	for _, id := range strings.Split(get("NANOCLAW_IMAGE_USERS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.ImageUsers[id] = true
		}
	}
	// Deploy-secret store: TTL backstop so a forgotten key doesn't linger.
	ttl := time.Duration(clampInt(get("NANOCLAW_SECRET_TTL_MIN", ""), 120, 1, 1440)) * time.Minute
	cfg.Secrets = NewSecretStore(cfg.DataDir, ttl)
	// Focus channels persisted at runtime via /focus (merged with the env list).
	for _, id := range loadFocus(cfg.DataDir) {
		cfg.FocusChannels[id] = true
	}
	if cfg.DiscordToken == "" {
		return nil, errors.New("DISCORD_TOKEN is required (nanoclaw.env or environment)")
	}
	if cfg.DeepseekKey == "" {
		return nil, errors.New("DEEPSEEK_API_KEY is required (nanoclaw.env or environment)")
	}
	return cfg, nil
}
