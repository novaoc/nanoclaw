package main

import (
	"strings"
	"testing"
)

// A REFUSED gated call ran nothing, so it must not poison the turn's
// injection-guard flags and block later web tools.
func TestRefusedCodeDoesNotPoisonTurn(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"admin": true}
	tc := &ToolCtx{cfg: cfg, authorID: "not-a-coder"}
	if out := tc.Run("shell", `{"command":"id"}`); !strings.Contains(out, "REFUSED") {
		t.Fatalf("expected refusal, got %q", out)
	}
	if tc.usedCode {
		t.Fatal("refused shell call must not set usedCode")
	}
	// github with no token configured is also a no-op — must not set the flag
	if out := tc.Run("github", `{"action":"create_repo","name":"x"}`); !strings.Contains(out, "isn't configured") {
		t.Fatalf("expected not-configured, got %q", out)
	}
	if tc.usedCode {
		t.Fatal("unconfigured github call must not set usedCode")
	}
}

// attach_image is an arbitrary outbound GET: it must count as web so a code
// turn can't use it as an exfiltration channel.
func TestAttachImageCountsAsWeb(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"admin": true}
	tc := &ToolCtx{cfg: cfg, authorID: "admin"}
	if out := tc.Run("shell", `{"command":"true"}`); strings.Contains(out, "REFUSED") {
		t.Fatalf("coder shell should run, got %q", out)
	}
	if out := tc.Run("attach_image", `{"url":"https://example.com/x.png"}`); !strings.Contains(out, "REFUSED") {
		t.Fatalf("attach_image after code must be refused, got %q", out)
	}
}

func TestChartMoneyMicroPrices(t *testing.T) {
	cases := map[float64]string{
		0.00001234: "$0.00001234",
		1.234:      "$1.234",
		0:          "$0",
	}
	for v, want := range cases {
		if got := chartMoney(v); got != want {
			t.Errorf("chartMoney(%v) = %q, want %q", v, got, want)
		}
	}
	if got := chartMoney(0.00001234); strings.ContainsAny(got, "eE") {
		t.Errorf("micro price rendered scientific: %q", got)
	}
}

func TestGhEsc(t *testing.T) {
	if got := ghEsc("docs/notes#1.md"); got != "docs/notes%231.md" {
		t.Errorf("ghEsc = %q", got)
	}
	if got := ghEsc("feature/x"); got != "feature/x" {
		t.Errorf("slashes must survive: %q", got)
	}
}
