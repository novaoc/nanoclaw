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

func TestIsLeakedToolCall(t *testing.T) {
	leak := `<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name="bench_chart">`
	if !isLeakedToolCall(leak) {
		t.Error("should flag DSML tool-call leak")
	}
	if isLeakedToolCall("Bitcoin is at $65k, up 1.6% this month.") {
		t.Error("normal prose must not be flagged")
	}
}

func TestSelfDatedMemory(t *testing.T) {
	cfg := testCfg(t)
	appendMemory(cfg, "- [2026-08-09] Aregus is on CET")
	appendMemory(cfg, "plain note")
	m := fullMemory(cfg)
	if strings.Contains(m, "] - [") {
		t.Errorf("double date not stripped: %q", m)
	}
	if !strings.Contains(m, "Aregus is on CET") || !strings.Contains(m, "plain note") {
		t.Errorf("notes missing: %q", m)
	}
}

func TestUnwrapArgs(t *testing.T) {
	// double-wrapped as a JSON string (the /dive poke-store failure)
	got := unwrapArgs(`{"arguments":"{\"name\":\"x.html\",\"content\":\"hi\"}"}`)
	if !strings.Contains(got, `"name":"x.html"`) {
		t.Errorf("string-wrapped not unwrapped: %q", got)
	}
	// double-wrapped as a nested object
	got = unwrapArgs(`{"arguments":{"name":"x.html","content":"hi"}}`)
	if !strings.Contains(got, `"name":"x.html"`) {
		t.Errorf("object-wrapped not unwrapped: %q", got)
	}
	// normal args pass through untouched
	normal := `{"name":"x.html","content":"hi"}`
	if unwrapArgs(normal) != normal {
		t.Errorf("normal args altered: %q", unwrapArgs(normal))
	}
	// a real single-field arg that isn't the wrapper passes through
	one := `{"query":"charizard"}`
	if unwrapArgs(one) != one {
		t.Errorf("legit single field altered: %q", unwrapArgs(one))
	}
}

func TestRunUnwrapsSaveArtifact(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	out := tc.Run("save_artifact", `{"arguments":"{\"name\":\"mock.html\",\"content\":\"<h1>hi</h1>\"}"}`)
	if !strings.Contains(out, "saved") || len(tc.Artifacts) != 1 {
		t.Fatalf("double-wrapped save_artifact should still save: %q (arts=%d)", out, len(tc.Artifacts))
	}
}
