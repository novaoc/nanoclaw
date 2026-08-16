package main

import (
	"strings"
	"testing"
	"time"
)

// This literal is duplicated in holodex's jobs_test.go on purpose: both
// clients pin the canonical to the same bytes independently, so a drift on
// either side fails that side's build instead of producing signatures the
// other side silently rejects.
func TestRefCanonicalMatchesHolodex(t *testing.T) {
	p := refRequest{
		Action: "verify", Repo: "Velaoc/pokemart",
		SHA:  "0123456789abcdef0123456789abcdef01234567",
		Name: "pokemart", Target: "test", Dockerfile: "Dockerfile",
		Port: 0, Exp: 1_800_000_000,
	}
	want := "holodex-ref-v1\nverify\nVelaoc/pokemart\n0123456789abcdef0123456789abcdef01234567" +
		"\npokemart\ntest\nDockerfile\n0\n1800000000\n"
	if got := p.canonical(); got != want {
		t.Fatalf("canonical drifted:\n%q\nwant\n%q", got, want)
	}
}

func TestRefHeadersCarryEverySignedField(t *testing.T) {
	p := refRequest{
		Action: "deploy", Repo: "a/b", SHA: strings.Repeat("ab", 20),
		Name: "app", Dockerfile: "Dockerfile", Port: 8080,
		Exp: time.Now().Unix() + 600,
	}
	h := p.headers("secret")
	for _, key := range []string{
		"X-Holodex-Repo", "X-Holodex-Sha", "X-Holodex-Name",
		"X-Holodex-Dockerfile", "X-Holodex-Port", "X-Holodex-Exp", "X-Holodex-Sign",
	} {
		if h[key] == "" {
			t.Errorf("%s missing from signed headers", key)
		}
	}
	if len(h["X-Holodex-Sign"]) != 64 {
		t.Errorf("signature is not a hex sha256: %q", h["X-Holodex-Sign"])
	}

	// Different secrets must sign differently.
	if p.headers("other")["X-Holodex-Sign"] == h["X-Holodex-Sign"] {
		t.Fatal("signature does not depend on the secret")
	}
}

func TestRenderVerifyResultBothOutcomes(t *testing.T) {
	pass := `{"ok":true,"receipt":"v1:1:d:test:aa","files":12,"duration_ms":90000,"logs":"fine"}`
	got := renderVerifyResult("a/b", "deadbeef", pass)
	if !strings.Contains(got, "Verification PASSED") || !strings.Contains(got, "receipt=v1:1:d:test:aa") {
		t.Fatalf("pass rendering: %s", got)
	}

	fail := `{"ok":false,"error":"verification failed","logs":"#19 1.0 Error:\r\n#19 1.0 SomeTest#x:\r\n#19 1.0 NoMethodError: nope\r\n"}`
	got = renderVerifyResult("a/b", "deadbeef", fail)
	if !strings.Contains(got, "Verification FAILED") || !strings.Contains(got, "NoMethodError") {
		t.Fatalf("fail rendering: %s", got)
	}

	// Unparseable bodies pass through rather than vanish.
	if got := renderVerifyResult("a/b", "d", "not json"); got != "not json" {
		t.Fatalf("passthrough broke: %q", got)
	}
}

func TestRenderDeployResult(t *testing.T) {
	ok := `{"url":"https://app-1234.demo.holode.xyz/","slug":"app-1234","kind":"container","expires":"2026-08-17T00:00:00Z"}`
	got := renderDeployResult("deadbeef", ok)
	if !strings.Contains(got, "https://app-1234.demo.holode.xyz/") || !strings.Contains(got, "deadbeef") {
		t.Fatalf("deploy rendering: %s", got)
	}
	if got := renderDeployResult("d", `{"error":"boom"}`); !strings.Contains(got, "boom") {
		t.Fatalf("error passthrough broke: %q", got)
	}
}
