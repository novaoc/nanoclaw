package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type repoArchiveParams struct {
	Action     string
	Name       string
	Target     string
	Dockerfile string
	Port       int
}

func (p repoArchiveParams) canonical() string {
	return strings.Join([]string{
		"holodeck-archive-v1",
		p.Action,
		p.Name,
		p.Target,
		p.Dockerfile,
		strconv.Itoa(p.Port),
		"",
	}, "\n")
}

func signArchive(secret string, p repoArchiveParams, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, p.canonical())
	if _, err := io.Copy(mac, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (tc *ToolCtx) verifyRepo(a toolArgs) string {
	if g := tc.repoBuildGate(a.Repo); g != "" {
		return g
	}
	target := strings.TrimSpace(a.Target)
	if target == "" {
		target = "test"
	}
	dockerfile := strings.TrimSpace(a.Dockerfile)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = a.Repo
	}
	p := repoArchiveParams{Action: "verify", Name: name, Target: target, Dockerfile: dockerfile}
	result, sha := tc.sendRepoArchive(a.Repo, a.Ref, "/api/verify", p, "")
	if sha == "" {
		return result
	}
	var out struct {
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
		Logs       string `json:"logs"`
		Receipt    string `json:"receipt"`
		DurationMS int64  `json:"duration_ms"`
		Files      int    `json:"files"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		return result
	}
	if !out.OK || out.Receipt == "" {
		return fmt.Sprintf("Verification FAILED for %s@%s: %s\n%s", a.Repo, sha, out.Error, clip(out.Logs, 6000))
	}
	logTail := clip(out.Logs, 3500)
	return fmt.Sprintf("Verification PASSED for %s@%s (%d files, %.1fs). Use deploy_repo with ref=%s and receipt=%s to deploy this exact tested source.\n%s",
		a.Repo, sha, out.Files, float64(out.DurationMS)/1000, sha, out.Receipt, logTail)
}

func (tc *ToolCtx) deployRepo(a toolArgs) string {
	if g := tc.repoBuildGate(a.Repo); g != "" {
		return g
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Ref) == "" || strings.TrimSpace(a.Receipt) == "" {
		return "deploy_repo needs name, the exact verified ref SHA, and the receipt returned by verify_repo."
	}
	p := repoArchiveParams{Action: "deploy", Name: strings.TrimSpace(a.Name), Dockerfile: "Dockerfile", Port: a.Port}
	result, sha := tc.sendRepoArchive(a.Repo, a.Ref, "/api/deploy/archive", p, strings.TrimSpace(a.Receipt))
	if sha == "" {
		return result
	}
	var out struct {
		URL     string `json:"url"`
		Slug    string `json:"slug"`
		Kind    string `json:"kind"`
		Expires string `json:"expires"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(result), &out); err != nil || out.URL == "" {
		return result
	}
	return fmt.Sprintf("Deployed tested commit %s at %s — the demo deck wipes daily (next: %s); the GitHub repo is permanent.", sha, out.URL, out.Expires)
}

func (tc *ToolCtx) repoBuildGate(repo string) string {
	if tc.cfg.SandboxURL == "" || tc.cfg.SandboxToken == "" || tc.cfg.SandboxSecret == "" {
		return "Holodeck repository builds aren't fully configured (NANOCLAW_SANDBOX_URL/TOKEN/SECRET)."
	}
	if tc.cfg.GitHubToken == "" {
		return "GitHub isn't configured, so Vela can't fetch the repository archive."
	}
	if g := tc.codeGate(); g != "" {
		return g
	}
	if strings.TrimSpace(repo) == "" {
		return "repository build needs repo='name' or repo='owner/name'."
	}
	tc.usedCode = true
	return ""
}

// sendRepoArchive downloads a private/public GitHub repository with Vela's
// token, then streams the archive to Holodeck. The token never leaves the Nano;
// only source bytes plus their provenance signature cross the boundary.
func (tc *ToolCtx) sendRepoArchive(repo, ref, endpoint string, p repoArchiveParams, receipt string) (string, string) {
	gh := newGH(tc.cfg)
	archive, sha, err := gh.downloadArchive(repo, strings.TrimSpace(ref), tc.cfg.DataDir)
	if err != nil {
		return "repository download failed: " + err.Error(), ""
	}
	defer os.Remove(archive)
	sig, err := signArchive(tc.cfg.SandboxSecret, p, archive)
	if err != nil {
		return "repository signing failed: " + err.Error(), ""
	}
	f, err := os.Open(archive)
	if err != nil {
		return "repository upload failed: " + err.Error(), ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "repository upload failed: " + err.Error(), ""
	}
	req, err := http.NewRequest(http.MethodPost, tc.cfg.SandboxURL+endpoint, f)
	if err != nil {
		return "repository upload failed: " + err.Error(), ""
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+tc.cfg.SandboxToken)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Holodeck-Sign", sig)
	req.Header.Set("X-Holodeck-Name", p.Name)
	req.Header.Set("X-Holodeck-Target", p.Target)
	req.Header.Set("X-Holodeck-Dockerfile", p.Dockerfile)
	if p.Port != 0 {
		req.Header.Set("X-Holodeck-Port", strconv.Itoa(p.Port))
	}
	if receipt != "" {
		req.Header.Set("X-Holodeck-Verify", receipt)
	}
	client := *ssrfClient
	client.Timeout = 20 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return "Holodeck request failed: " + err.Error(), ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("Holodeck %d for %s@%s: %.12000s", resp.StatusCode, repo, sha, raw), ""
	}
	return string(raw), sha
}
