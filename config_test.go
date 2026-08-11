package main

import (
	"os"
	"testing"
)

// The rename keeps the board's existing NANOCLAW_* configuration working
// while preferring the VELA_* spellings everywhere.
func TestLoadConfigPrefersVelaKeysAndFallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	t.Setenv("DISCORD_TOKEN", "d")
	t.Setenv("DEEPSEEK_API_KEY", "k")
	t.Setenv("NANOCLAW_MODEL", "legacy-model")
	t.Setenv("NANOCLAW_SANDBOX_URL", "https://legacy.example/")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "legacy-model" || cfg.SandboxURL != "https://legacy.example" {
		t.Fatalf("legacy NANOCLAW_* keys were dropped: model=%q sandbox=%q", cfg.Model, cfg.SandboxURL)
	}

	t.Setenv("VELA_MODEL", "current-model")
	t.Setenv("VELA_SANDBOX_URL", "https://current.example")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "current-model" || cfg.SandboxURL != "https://current.example" {
		t.Fatalf("VELA_* keys must win over NANOCLAW_*: model=%q sandbox=%q", cfg.Model, cfg.SandboxURL)
	}
}

// vela.env is read in addition to the legacy nanoclaw.env file (later wins).
func TestLoadConfigReadsVelaEnvFile(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	t.Setenv("DISCORD_TOKEN", "d")
	t.Setenv("DEEPSEEK_API_KEY", "k")
	if err := os.WriteFile("nanoclaw.env", []byte("NANOCLAW_MODEL=from-legacy-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("vela.env", []byte("VELA_MODEL=from-vela-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "from-vela-file" {
		t.Fatalf("vela.env not preferred: %q", cfg.Model)
	}
}
