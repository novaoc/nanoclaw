package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnedThreadsPersistAcrossRestart(t *testing.T) {
	cfg := testCfg(t)
	first := NewOwnedThreads(cfg)
	first.Add("thread-123")

	second := NewOwnedThreads(cfg)
	if !second.Has("thread-123") {
		t.Fatal("Vela forgot a forum thread she created after restart")
	}
	if second.Has("someone-elses-thread") {
		t.Fatal("unowned forum thread was treated as Vela's")
	}
}

func TestForumThreadHistoryCarriesPlanIntoApproval(t *testing.T) {
	cfg := testCfg(t)
	a := NewAgent(cfg)
	a.SeedForumThread(
		"thread-123",
		"aregus",
		"build an Instagram Pokemon storefront",
		"Goal: working storefront. Plan: build, test, publish. Reply go ahead to start.",
	)

	// Re-open through a fresh Agent to prove the handoff is on disk, not just
	// held in the process that created the Discord post.
	restarted := NewAgent(cfg)
	got := restarted.hist.Get("thread-123")
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("seeded request history wrong: %+v", got)
	}
	if !strings.Contains(got[0].Content, "Instagram Pokemon storefront") ||
		!strings.Contains(got[1].Content, "Reply go ahead") {
		t.Fatalf("request or plan missing from seeded history: %+v", got)
	}
}

func TestApprovalLanguageIsExplicit(t *testing.T) {
	for _, phrase := range []string{
		`"go ahead"`,
		"treat it as authorization and begin the work",
		`Do not ask "go ahead with what?"`,
		"deliver the repository and",
	} {
		if !strings.Contains(systemPrompt, phrase) {
			t.Fatalf("request approval guidance missing %q", phrase)
		}
	}
}

func TestApprovalTurnActuallyReceivesSeededPlan(t *testing.T) {
	var seen []Msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = req.Messages
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"starting the approved build"}}]}`))
	}))
	defer srv.Close()

	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	a := NewAgent(cfg)
	a.SeedForumThread("thread-123", "aregus", "build the store", "Plan: build, test, and publish it.")
	a.Handle("thread-123", "u1", "aregus", "go ahead")

	joined := ""
	for _, msg := range seen {
		joined += "\n" + msg.Role + ": " + msg.Content
	}
	planAt := strings.Index(joined, "Plan: build, test, and publish it.")
	approvalAt := strings.Index(joined, "aregus: go ahead")
	if planAt < 0 || approvalAt < 0 || planAt > approvalAt {
		t.Fatalf("approval turn did not receive its preceding plan: %s", joined)
	}
}
