package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempWS(t *testing.T) *workspace {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir()) // macOS /var → /private/var
	if err != nil {
		t.Fatal(err)
	}
	return &workspace{root: root, repo: "Velaoc/app"}
}

func TestResolveKeepsPathsInsideWorkspace(t *testing.T) {
	ws := tempWS(t)
	ok := []string{"a.rb", "app/models/book.rb", "./config/routes.rb", "deep/nested/dir/file"}
	for _, p := range ok {
		if _, err := ws.resolve(p); err != nil {
			t.Errorf("resolve(%q) rejected a legit path: %v", p, err)
		}
	}
}

func TestResolveRefusesEscapes(t *testing.T) {
	ws := tempWS(t)
	bad := []string{
		"../secret",
		"../../etc/passwd",
		"app/../../escape",
		"/etc/passwd",
		"a\x00b",
	}
	for _, p := range bad {
		if _, err := ws.resolve(p); err == nil {
			t.Errorf("resolve(%q) should have been refused", p)
		}
	}
}

func TestResolveRefusesSymlinkEscape(t *testing.T) {
	ws := tempWS(t)
	outside := t.TempDir()
	link := filepath.Join(ws.root, "sneaky")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// Writing "through" the symlink must be refused.
	if _, err := ws.resolve("sneaky/loot"); err == nil {
		t.Fatal("a symlink pointing outside the workspace was accepted")
	}
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	ws := tempWS(t)
	if err := ws.writeFile("app/models/book.rb", "class Book; end\n"); err != nil {
		t.Fatal(err)
	}
	got, err := ws.readFile("app/models/book.rb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "class Book") {
		t.Fatalf("round trip lost content: %q", got)
	}
}

func TestWriteRefusesEscape(t *testing.T) {
	ws := tempWS(t)
	if err := ws.writeFile("../loot", "x"); err == nil {
		t.Fatal("write escaped the workspace")
	}
}

// The load-bearing guarantee: the shell the model drives never sees the
// process secrets.
func TestRunShellStripsSecretsFromEnv(t *testing.T) {
	ws := tempWS(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-should-not-leak")
	t.Setenv("GITHUB_TOKEN", "ghp-should-not-leak")
	t.Setenv("HOLODEX_TOKEN", "holodex-should-not-leak")

	out, err := ws.runShell("env", 10*time.Second)
	if err != nil {
		t.Fatalf("env failed: %v\n%s", err, out)
	}
	for _, secret := range []string{"sk-should-not-leak", "ghp-should-not-leak", "holodex-should-not-leak"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret leaked into the sandbox env: %s", secret)
		}
	}
}

func TestRunShellRunsInWorkspace(t *testing.T) {
	ws := tempWS(t)
	if err := ws.writeFile("marker.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	out, err := ws.runShell("ls", 10*time.Second)
	if err != nil {
		t.Fatalf("ls failed: %v", err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Fatalf("shell not in workspace cwd: %q", out)
	}
}

func TestRunShellTimesOut(t *testing.T) {
	ws := tempWS(t)
	_, err := ws.runShell("sleep 5", 1*time.Second)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestListTreeSkipsGit(t *testing.T) {
	ws := tempWS(t)
	_ = ws.writeFile(".git/config", "secret")
	_ = ws.writeFile("app/real.rb", "code")
	out, err := ws.listTree(400)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".git") {
		t.Fatalf(".git leaked into the tree listing:\n%s", out)
	}
	if !strings.Contains(out, "app/real.rb") {
		t.Fatalf("real file missing from tree:\n%s", out)
	}
}

func TestScrubRemovesSecret(t *testing.T) {
	if got := scrub("token=ghp-abc123 failed", "ghp-abc123"); strings.Contains(got, "ghp-abc123") {
		t.Fatalf("scrub left the secret: %q", got)
	}
	if got := scrub("nothing here", ""); got != "nothing here" {
		t.Fatalf("empty-secret scrub altered the string: %q", got)
	}
}

func TestDistillMinitestKeepsFailuresAndSummary(t *testing.T) {
	raw := strings.Repeat("Run options: --seed 1234\n........\n", 200) +
		"Failure:\nBillingTest#test_checkout [test/integration/billing_test.rb:107]:\n--- expected\n+++ actual\n-\"https://example.com/billing\"\n+\"https://blankpage.api.holode.xyz/billing\"\n\n" +
		strings.Repeat(".....\n", 400) +
		"Error:\nStorefrontCheckoutServicesTest#test_origin:\nFoundation::RuntimeConfig::Invalid: APP_HOST must be the configured domain\n\n" +
		"396 runs, 3101 assertions, 5 failures, 1 errors, 8 skips\n"
	got := distillMinitest(raw, 6000)
	if !strings.Contains(got, "396 runs") {
		t.Fatalf("summary lost:\n%s", got)
	}
	if !strings.Contains(got, "blankpage.api.holode.xyz") || !strings.Contains(got, "RuntimeConfig::Invalid") {
		t.Fatalf("failure blocks lost:\n%s", got)
	}
	if len(got) > 6200 {
		t.Fatalf("budget blown: %d bytes", len(got))
	}
	if strings.Count(got, "....") > 2 {
		t.Fatalf("distilled output still full of progress dots")
	}
}
