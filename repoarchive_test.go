package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoArchiveCanonicalAndSignature(t *testing.T) {
	p := repoArchiveParams{Action: "verify", Name: "Store", Target: "test", Dockerfile: "Dockerfile", Port: 3000}
	wantCanonical := "holodeck-archive-v1\nverify\nStore\ntest\nDockerfile\n3000\n"
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

func TestRailsTemplateActionIsPrivateAndDoesNotExposeTemplate(t *testing.T) {
	cfg := testCfg(t)
	cfg.GitHubToken = "github-token"
	cfg.RailsTemplate = "private-owner/private-foundation"

	var description string
	for _, d := range toolDefs(cfg) {
		if d.Function.Name == "github" {
			description = d.Function.Description
		}
	}
	if !strings.Contains(description, "create_rails_app") || !strings.Contains(description, "ALWAYS-PRIVATE") {
		t.Fatal("GitHub tool does not expose the private Rails factory")
	}
	if strings.Contains(description, cfg.RailsTemplate) {
		t.Fatal("private foundation repository leaked into the model-facing tool description")
	}
}
