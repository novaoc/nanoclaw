package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// deploy_demo — Vela's own demo hosting (the holodeck, cmd/holodeck). She
// POSTs an app's files to the sandbox and gets a live URL on its domain;
// every demo self-destructs after 7 days, so the GitHub repo (github tool)
// is the permanent copy. Static files only — nothing she deploys executes on
// the sandbox server.

func (tc *ToolCtx) deployDemo(a toolArgs) string {
	if tc.cfg.SandboxURL == "" || tc.cfg.SandboxToken == "" {
		return "demo hosting isn't configured on this instance (NANOCLAW_SANDBOX_URL/TOKEN)."
	}
	if !tc.cfg.RepoAllowed(tc.authorID) {
		return "REFUSED: deploys are limited to an allowlist here (NANOCLAW_REPO_USERS), and this user isn't on it."
	}
	if strings.TrimSpace(a.Name) == "" || len(a.Files) == 0 {
		return "deploy error: needs a name and files (an index.html at minimum)."
	}
	hasIndex := false
	for _, f := range a.Files {
		if f.Path == "index.html" {
			hasIndex = true
		}
	}
	if !hasIndex {
		return "deploy error: include an index.html at the root — it's the homepage."
	}
	tc.usedCode = true // outbound deploy, same injection-guard side as github
	body, err := json.Marshal(map[string]any{"name": a.Name, "files": a.Files})
	if err != nil {
		return "deploy error: " + err.Error()
	}
	req, err := http.NewRequest("POST", tc.cfg.SandboxURL+"/api/deploy", bytes.NewReader(body))
	if err != nil {
		return "deploy error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+tc.cfg.SandboxToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ssrfClient.Do(req)
	if err != nil {
		return "deploy error: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("deploy failed — holodeck %d: %.300s", resp.StatusCode, raw)
	}
	var out struct{ URL, Slug, Expires string }
	if json.Unmarshal(raw, &out) != nil || out.URL == "" {
		return "deploy failed — holodeck sent back something unusable."
	}
	log.Printf("holodeck deploy by=%s name=%q slug=%s", tc.authorID, a.Name, out.Slug)
	return fmt.Sprintf("Live at %s — this demo self-destructs on %s (repos are the permanent copy).", out.URL, out.Expires)
}
