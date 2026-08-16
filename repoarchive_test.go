package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoArchiveCanonicalAndSignature(t *testing.T) {
	p := repoArchiveParams{Action: "verify", Name: "Store", Target: "test", Dockerfile: "Dockerfile", Port: 3000}
	wantCanonical := "holodex-archive-v1\nverify\nStore\ntest\nDockerfile\n3000\n"
	if got := p.canonical(); got != wantCanonical {
		t.Fatalf("canonical mismatch:\n%q\nwant:\n%q", got, wantCanonical)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "repo.tar.gz")
	body := []byte("archive bytes")
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := signArchive("secret", p, archive)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(wantCanonical))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signature=%s want=%s", got, want)
	}
}

func TestRepoBuildToolsAreCoderOnlyAndConfigured(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "github-token"
	cfg.SandboxURL = "https://demo.example"
	cfg.SandboxToken = "sandbox-token"
	cfg.SandboxSecret = "sandbox-secret"
	cfg.Coders = map[string]bool{"coder": true}

	names := map[string]string{}
	for _, d := range toolDefs(cfg) {
		names[d.Function.Name] = d.Function.Description
	}
	for _, name := range []string{"verify_repo", "deploy_repo"} {
		if names[name] == "" {
			t.Fatalf("%s missing from configured coder tool belt", name)
		}
	}
	if !strings.Contains(names["deploy_repo"], "exact commit SHA") || !strings.Contains(names["deploy_repo"], "rejects changed") {
		t.Fatal("deploy_repo does not explain the verified-source gate")
	}

	noncoder := &ToolCtx{cfg: cfg, authorID: "stranger"}
	if out := noncoder.verifyRepo(toolArgs{Repo: "owner/app"}); !strings.Contains(out, "REFUSED") {
		t.Fatalf("non-coder repository build was not refused: %s", out)
	}
	coder := &ToolCtx{cfg: cfg, authorID: "coder"}
	if out := coder.deployRepo(toolArgs{Repo: "owner/app"}); !strings.Contains(out, "needs name") {
		t.Fatalf("deploy_repo accepted missing verification inputs: %s", out)
	}
}

func TestRailsTemplateActionCreatesPublishableAppsWithoutExposingTemplate(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "github-token"
	cfg.RailsTemplate = "private-owner/private-foundation"

	var description string
	for _, d := range toolDefs(cfg) {
		if d.Function.Name == "github" {
			description = d.Function.Description
		}
	}
	// Applications are generated private and become public only through the
	// operator-gated publish step, so the model must be told both halves.
	// Spec stage comes first (app_spec tool); create_rails_app still scaffolds.
	if !strings.Contains(description, "create_rails_app") || !strings.Contains(description, "app_spec") || !strings.Contains(description, "PRIVATE") {
		t.Fatal("GitHub tool does not enforce the Rails application path")
	}
	if !strings.Contains(description, "publish_app") || !strings.Contains(description, "public and forkable") {
		t.Fatal("GitHub tool does not describe the gated publication step")
	}
	if !strings.Contains(description, "REPLACES THE ENTIRE FILE") || !strings.Contains(description, "delete_file") {
		t.Fatal("GitHub tool does not explain safe full-file writes and deletion")
	}
	if strings.Contains(description, cfg.RailsTemplate) {
		t.Fatal("private foundation repository leaked into the model-facing tool description")
	}
}

func TestRailsTemplateBlocksNonRailsApplicationPaths(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "not-used"
	cfg.RailsTemplate = "private-owner/private-foundation"
	cfg.SandboxURL = "https://demo.example"
	cfg.SandboxToken = "not-used"
	cfg.SandboxSecret = "not-used"

	tc := &ToolCtx{cfg: cfg, authorID: "coder"}
	if out := tc.runGithub(toolArgs{Action: "create_repo", Name: "static-app"}); !strings.Contains(out, "Rails-only") {
		t.Fatalf("generic app repository was not refused: %s", out)
	}
	if out := tc.deployDemo(toolArgs{Name: "static-app", Files: []demoFile{{Path: "index.html", Content: "hi"}}}); !strings.Contains(out, "Rails-only") {
		t.Fatalf("static app deployment was not refused: %s", out)
	}
	if tc.usedCode {
		t.Fatal("refused non-Rails paths must not mark code as executed")
	}
}

func TestFailureExcerptShortLogPassesThrough(t *testing.T) {
	logs := "everything fits"
	if got := failureExcerpt(logs, 6000); got != logs {
		t.Fatalf("short log should be untouched, got %q", got)
	}
}

// The regression this guards: a Rails verification log opens with kilobytes of
// package-install noise, and the minitest Error blocks live near the end.
// Clipping from the front hid the real error from the model entirely.
func TestFailureExcerptSurfacesMinitestErrors(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("#18 9.48 Unpacking libicu76:amd64 (76.1-4) ...\r\n")
	}
	b.WriteString("#19 51.99 Error:\r\n")
	b.WriteString("#19 51.99 HomePageTest#test_signed-in_history_page_lists_sessions:\r\n")
	b.WriteString("#19 51.99 NoMethodError: undefined method 'sign_in' for an instance of HomePageTest\r\n")
	b.WriteString("#19 51.99     test/integration/home_page_test.rb:40:in 'block in <class:HomePageTest>'\r\n")
	b.WriteString("#19 51.99 \r\n")
	for i := 0; i < 100; i++ {
		b.WriteString("#19 74.37 321 runs, 2510 assertions, 0 failures, 2 errors, 12 skips\r\n")
	}
	b.WriteString("ERROR: failed to build: failed to solve: process did not complete\n")

	got := failureExcerpt(b.String(), 6000)
	if len(got) > 6200 {
		t.Fatalf("excerpt blew the budget: %d bytes", len(got))
	}
	if !strings.Contains(got, "NoMethodError: undefined method 'sign_in'") {
		t.Fatalf("excerpt lost the actual test error:\n%s", got)
	}
	if !strings.Contains(got, "failed to build") {
		t.Fatalf("excerpt lost the log tail:\n%s", got)
	}
	if strings.Count(got, "Unpacking libicu76") > 20 {
		t.Fatalf("excerpt is still mostly package noise")
	}
}

func TestFailureExcerptDeduplicatesRepeatedBlocks(t *testing.T) {
	block := "#19 52.0 Error:\r\n#19 52.0 SomeTest#test_x:\r\n#19 52.0 NoMethodError: nope\r\n#19 52.0 \r\n"
	logs := strings.Repeat("#18 1.0 noise\r\n", 500) + block + strings.Repeat("#18 2.0 more noise\r\n", 200) + block
	got := failureExcerpt(logs, 4000)
	if n := strings.Count(got, "Test failures:"); n != 1 {
		t.Fatalf("want one failure section, got %d", n)
	}
	// The block appears once in the distilled section; the tail may echo it.
	if !strings.Contains(got, "NoMethodError: nope") {
		t.Fatalf("lost the error block:\n%s", got)
	}
}

func TestStripBuildPrefix(t *testing.T) {
	cases := map[string]string{
		"#19 51.99 Error:":       "Error:",
		"#19 51.99     indented": "    indented",
		"#7 DONE 1.4s":           "DONE 1.4s",
		"plain line":             "plain line",
		"#notanumber stays":      "#notanumber stays",
		"ERROR: failed to build": "ERROR: failed to build",
	}
	for in, want := range cases {
		if got := stripBuildPrefix(in); got != want {
			t.Errorf("stripBuildPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed verification arrives as HTTP 422 with the build log embedded in
// the JSON body — the path the 2026-08-14 pokemart build exposed: the raw
// JSON head was returned verbatim and the brakeman gate failure at the tail
// was never seen by the model.
func TestHolodexFailureMessageDistills422Logs(t *testing.T) {
	logs := strings.Repeat("#18 9.48 Unpacking libicu76:amd64 (76.1-4) ...\r\n", 400) +
		"#19 30.72 Brakeman 8.0.5 is not the latest version 8.0.6\r\n" +
		"ERROR: failed to build: failed to solve: process did not complete\n"
	body, err := json.Marshal(map[string]any{"error": "verification failed", "logs": logs})
	if err != nil {
		t.Fatal(err)
	}
	got := holodexFailureMessage(422, "pokemart", "abc1234", body)
	if !strings.Contains(got, "Brakeman 8.0.5 is not the latest") {
		t.Fatalf("422 message lost the actual gate failure:\n%.500s", got)
	}
	if strings.Contains(got, `\r\n`) {
		t.Fatalf("message still contains raw JSON escapes:\n%.300s", got)
	}
	if len(got) > 7000 {
		t.Fatalf("message too long for the model: %d bytes", len(got))
	}
}

func TestHolodexFailureMessageFallsBackOnNonJSON(t *testing.T) {
	got := holodexFailureMessage(502, "r", "s", []byte("bad gateway"))
	if !strings.Contains(got, "Holodex 502") || !strings.Contains(got, "bad gateway") {
		t.Fatalf("fallback lost the body: %q", got)
	}
}
