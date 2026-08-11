package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// deploy_demo — Vela's own demo hosting (the Holodex,
// github.com/novaoc/holodex). She POSTs an app's files to the sandbox and gets
// a live URL on its domain; the whole deck wipes daily, so the GitHub repo
// (github tool) is the permanent copy. An app is either a static bundle (an
// index.html) or a real app (include a Dockerfile — Node, Python, Go, …). Every
// deploy is HMAC-signed with the shared build secret so Holodex can prove it
// came from Vela's own pipeline and refuse anything else.

func (tc *ToolCtx) deployDemo(a toolArgs) string {
	if tc.cfg.RailsTemplate != "" {
		return "deploy error: Vela applications are Rails-only; push the Rails repository, pass verify_repo, then deploy_repo with the exact receipt"
	}
	if tc.cfg.SandboxURL == "" || tc.cfg.SandboxToken == "" || tc.cfg.SandboxSecret == "" {
		return "demo hosting isn't fully configured on this instance (NANOCLAW_SANDBOX_URL/TOKEN/SECRET)."
	}
	if !tc.cfg.RepoAllowed(tc.authorID) {
		return "REFUSED: deploys are limited to an allowlist here (NANOCLAW_REPO_USERS), and this user isn't on it."
	}
	if strings.TrimSpace(a.Name) == "" || len(a.Files) == 0 {
		return "deploy error: needs a name and files."
	}
	hasIndex, hasDockerfile := false, false
	for _, f := range a.Files {
		switch f.Path {
		case "index.html":
			hasIndex = true
		case "Dockerfile":
			hasDockerfile = true
		}
	}
	if !hasIndex && !hasDockerfile {
		return "deploy error: include a Dockerfile (to run an app) or an index.html at the root (static site)."
	}
	tc.usedCode = true // outbound deploy, same injection-guard side as github
	payload := map[string]any{"name": a.Name, "files": a.Files}
	if a.Port != 0 {
		payload["port"] = a.Port
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "deploy error: " + err.Error()
	}
	req, err := http.NewRequest("POST", tc.cfg.SandboxURL+"/api/deploy", bytes.NewReader(body))
	if err != nil {
		return "deploy error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+tc.cfg.SandboxToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Holodex-Sign", signBody(tc.cfg.SandboxSecret, body)) // provenance
	resp, err := ssrfClient.Do(req)
	if err != nil {
		return "deploy error: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("deploy failed — holodex %d: %.500s", resp.StatusCode, raw)
	}
	var out struct{ URL, Slug, Kind, Expires string }
	if json.Unmarshal(raw, &out) != nil || out.URL == "" {
		return "deploy failed — Holodex sent back something unusable."
	}
	log.Printf("holodex deploy by=%s name=%q slug=%s kind=%s", tc.authorID, a.Name, out.Slug, out.Kind)
	return fmt.Sprintf("Live at %s — the demo deck wipes daily (next: %s), so the repo is the permanent copy.", out.URL, out.Expires)
}

// signBody is the HMAC-SHA256 provenance signature Holodex verifies.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
