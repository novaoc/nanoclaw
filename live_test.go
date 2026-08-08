package main

import (
	"os"
	"strings"
	"testing"
)

// Live smoke against the real provider — runs only when NANOCLAW_LIVE=1
// and a key is configured (nanoclaw.env or environment). Exercises the
// FULL loop: search tool, artifact tool, memory, and a /dive with the
// self-review pass.
func TestLiveAgentLoop(t *testing.T) {
	if os.Getenv("NANOCLAW_LIVE") != "1" {
		t.Skip("set NANOCLAW_LIVE=1 for the live provider smoke")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Skipf("no config: %v", err)
	}
	cfg.DataDir = t.TempDir()
	for _, d := range []string{"/artifacts", "/history"} {
		_ = os.MkdirAll(cfg.DataDir+d, 0o755)
	}
	a := NewAgent(cfg)

	t.Run("mockup+artifact", func(t *testing.T) {
		r := a.Handle("live-test", "wren", "make a tiny self-contained HTML page that says NANOCLAW LIVE in huge letters — save it as an artifact")
		t.Logf("reply: %.400s", r.Text)
		if len(r.Artifacts) == 0 {
			t.Fatalf("expected an artifact, got none — reply: %s", r.Text)
		}
		b, _ := os.ReadFile(r.Artifacts[0])
		if !strings.Contains(strings.ToUpper(string(b)), "NANOCLAW") {
			t.Fatalf("artifact content unexpected: %.200s", b)
		}
		t.Logf("artifact %s (%d bytes) ✓", r.Artifacts[0], len(b))
	})

	t.Run("dive+search", func(t *testing.T) {
		r := a.Dive("live-test", "wren", "what model releases did DeepSeek ship most recently? one line, cite a URL you actually fetched")
		t.Logf("dive reply: %.600s", r.Text)
		if !strings.Contains(r.Text, "http") {
			t.Errorf("expected a cited URL in the dive reply")
		}
	})
}
