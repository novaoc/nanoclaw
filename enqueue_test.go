package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"testing"
)

// The ticket the board mints must be byte-identical to what holodex's
// parseTicketHeader expects — same canonical, same field order. This pins the
// board side; holodex's jobs_test pins the server side.
func TestBuildTicketCanonicalMatchesHolodex(t *testing.T) {
	secret := "build-secret"
	job, repo, max, exp := "wj-abc", "Velaoc/app", 6, int64(1_800_000_000)

	got := signBuildTicket(secret, job, repo, max, exp)

	// Recompute the way holodex does.
	canonical := strings.Join([]string{
		"holodex-ticket-v1", job, repo, strconv.Itoa(max), strconv.FormatInt(exp, 10), "",
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, canonical)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("ticket signature drift:\n%s\nwant\n%s", got, want)
	}
}

func TestEnqueueBuildRequiresWorkerConfig(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"coder": true}
	// No worker configured.
	tc := &ToolCtx{cfg: cfg, authorID: "coder"}
	out := tc.enqueueBuild(toolArgs{Repo: "Velaoc/app", Name: "app"})
	if !strings.Contains(out, "worker isn't configured") {
		t.Fatalf("expected a not-configured message, got: %s", out)
	}
}

func TestEnqueueBuildValidatesArgs(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"coder": true}
	cfg.WorkerURL = "https://worker.example"
	cfg.WorkerToken = "wt"
	cfg.SandboxSecret = "s"
	cfg.GitHubToken = "gh"

	tc := &ToolCtx{cfg: cfg, authorID: "coder"}
	if out := tc.enqueueBuild(toolArgs{Name: "app"}); !strings.Contains(out, "needs the repo") {
		t.Fatalf("missing-repo not caught: %s", out)
	}

	// A non-coder is refused before any network work.
	nc := &ToolCtx{cfg: cfg, authorID: "stranger"}
	if out := nc.enqueueBuild(toolArgs{Repo: "Velaoc/app", Name: "app"}); !strings.Contains(out, "REFUSED") {
		t.Fatalf("non-coder enqueue not refused: %s", out)
	}
}
