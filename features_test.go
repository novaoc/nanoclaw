package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretNameNormalize(t *testing.T) {
	cases := map[string]string{
		"HETZNER_TOKEN": "HETZNER_TOKEN",
		"Hetzner API":   "HETZNER_API",
		"aws-key":       "AWSKEY",
		"  spaces  ":    "SPACES",
		"123bad":        "BAD",
		"!!!":           "",
	}
	for in, want := range cases {
		if got := secretName(in); got != want {
			t.Errorf("secretName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecretStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := NewSecretStore(dir, time.Hour)
	if _, err := s.Set("Hetzner Token", "secret-value-123"); err != nil {
		t.Fatal(err)
	}
	names := s.Names()
	if len(names) != 1 || names[0] != "HETZNER_TOKEN" {
		t.Fatalf("names = %v", names)
	}
	// value must be retrievable ONLY via EnvPairs, and the file must be 0600
	env := s.EnvPairs()
	if len(env) != 1 || env[0] != "HETZNER_TOKEN=secret-value-123" {
		t.Fatalf("envpairs = %v", env)
	}
	fi, err := os.Stat(filepath.Join(dir, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secrets file perms = %v, want 0600", fi.Mode().Perm())
	}
	// persistence across a reload
	s2 := NewSecretStore(dir, time.Hour)
	if got := s2.EnvPairs(); len(got) != 1 {
		t.Errorf("reloaded store lost the secret: %v", got)
	}
	// clear wipes memory and disk
	if n := s2.Clear(); n != 1 {
		t.Errorf("clear returned %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); !os.IsNotExist(err) {
		t.Error("secrets file should be gone after clear")
	}
}

func TestSecretTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	s := NewSecretStore(dir, time.Millisecond)
	s.Set("K", "v")
	time.Sleep(5 * time.Millisecond)
	if names := s.Names(); len(names) != 0 {
		t.Errorf("expected TTL expiry, still have %v", names)
	}
}

func TestNilSecretStoreSafe(t *testing.T) {
	var s *SecretStore
	if s.Names() != nil || s.EnvPairs() != nil || s.Clear() != 0 {
		t.Error("nil SecretStore must behave as empty")
	}
}

func TestParseDur(t *testing.T) {
	cases := map[string]time.Duration{
		"10m": 10 * time.Minute,
		"1h":  time.Hour,
		"1d":  24 * time.Hour,
		"90s": 90 * time.Second,
	}
	for in, want := range cases {
		got, err := parseDur(in)
		if err != nil || got != want {
			t.Errorf("parseDur(%q) = %v,%v want %v", in, got, err, want)
		}
	}
	if d, _ := parseDur("400d"); d > 28*24*time.Hour {
		t.Errorf("timeout not capped at 28d: %v", d)
	}
	if _, err := parseDur("nonsense"); err == nil {
		t.Error("expected error on bad duration")
	}
}

func TestIdFromMention(t *testing.T) {
	for _, in := range []string{"<@123456789>", "<@!123456789>", "123456789"} {
		if got := idFromMention(in); got != "123456789" {
			t.Errorf("idFromMention(%q) = %q", in, got)
		}
	}
}

func TestIsSnowflake(t *testing.T) {
	if !isSnowflake("975118321907269734") {
		t.Error("valid id not recognized")
	}
	if isSnowflake("introductions") {
		t.Error("channel name misread as id")
	}
}

func TestModerationGatedWithoutMods(t *testing.T) {
	// moderate must refuse cleanly when there's no actuator/guild (eval/DM)
	tc := &ToolCtx{cfg: testCfg(t)}
	out := tc.Run("moderate", `{"action":"kick","user":"<@1>"}`)
	if !strings.Contains(out, "isn't available") {
		t.Errorf("expected unavailable, got %q", out)
	}
}

// Generated applications must never be born public: GitHub's generate endpoint
// copies the foundation verbatim, so publication is a separate step that the
// operator controls. With publication off, publish_app must refuse BEFORE any
// API call — a network attempt here would mean the gate leaks.
func TestPublishAppRefusedWhenPublicationDisabled(t *testing.T) {
	g := &ghClient{token: "unusable-token"}
	out := g.publishApp("Velaoc/some-app", false)
	if !strings.Contains(out, "publishing is turned off") {
		t.Fatalf("disabled publication must refuse locally, got %q", out)
	}
	if strings.Contains(out, "github error") {
		t.Fatalf("gate must short-circuit before contacting GitHub, got %q", out)
	}
}

// The containment default matters more than the flag: an instance that never
// sets the variable must not publish.
func TestPublicAppsDefaultsOff(t *testing.T) {
	cfg := testCfg(t)
	if cfg.PublicApps {
		t.Fatal("generated applications must default to private on a fresh instance")
	}
}
