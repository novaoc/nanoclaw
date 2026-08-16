package main

// Git and Holodex client operations for the worker. The GitHub token and the
// Holodex bearer are used ONLY here, in the daemon process — they are never
// placed in the sandbox environment the model's shell sees.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cloneRepo does a shallow clone into the workspace using the scoped push
// token in the remote URL. The token is stripped from the stored remote
// afterwards so it never lingers in .git/config inside the sandbox.
func (s *server) cloneRepo(ws *workspace) error {
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", s.cfg.GitHubToken, ws.repo)
	if out, err := gitRun(ws.root, "", "clone", "--depth", "1", url, "."); err != nil {
		return fmt.Errorf("clone failed: %s", scrub(out, s.cfg.GitHubToken))
	}
	// Replace the credentialed remote with a clean one.
	clean := fmt.Sprintf("https://github.com/%s.git", ws.repo)
	_, _ = gitRun(ws.root, "", "remote", "set-url", "origin", clean)
	return nil
}

// commitAll stages everything and commits. Returns the new HEAD sha.
func (s *server) commitAll(ws *workspace, message string) (string, error) {
	if _, err := gitRun(ws.root, "", "add", "-A"); err != nil {
		return "", err
	}
	// Nothing staged → return current HEAD unchanged.
	if _, err := gitRun(ws.root, "", "diff", "--cached", "--quiet"); err == nil {
		return s.headSHA(ws)
	}
	out, err := gitRun(ws.root, "",
		"-c", "user.name=vela-worker",
		"-c", "user.email=worker@velaoc.users.noreply.github.com",
		"commit", "-m", message)
	if err != nil {
		return "", fmt.Errorf("commit failed: %s", out)
	}
	return s.headSHA(ws)
}

func (s *server) headSHA(ws *workspace) (string, error) {
	out, err := gitRun(ws.root, "", "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// push sends HEAD to origin/main using the token, which is injected only for
// this one command via an ephemeral remote URL.
func (s *server) push(ws *workspace) error {
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", s.cfg.GitHubToken, ws.repo)
	if out, err := gitRun(ws.root, "", "push", url, "HEAD:main"); err != nil {
		return fmt.Errorf("push failed: %s", scrub(out, s.cfg.GitHubToken))
	}
	return nil
}

// gitRun executes git with a stripped environment and a timeout.
func gitRun(dir, _ string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + dir,
		"GIT_TERMINAL_PROMPT=0", // never block on a credential prompt
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// ── Holodex ticket-verify client ────────────────────────────────────────────

type verifyResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Logs       string `json:"logs"`
	Receipt    string `json:"receipt"`
	Files      int    `json:"files"`
	DurationMS int64  `json:"duration_ms"`
}

// ticketVerify submits a reference verification authorized by the job ticket
// (not the build secret, which the worker does not have) and polls to
// completion.
func (s *server) ticketVerify(job *buildJob, sha string) (verifyResult, error) {
	req, err := http.NewRequest(http.MethodPost, s.cfg.HolodexURL+"/api/verify/ref", nil)
	if err != nil {
		return verifyResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.HolodexToken)
	req.Header.Set("X-Holodex-Repo", job.Repo)
	req.Header.Set("X-Holodex-Sha", sha)
	req.Header.Set("X-Holodex-Name", job.Name)
	req.Header.Set("X-Holodex-Dockerfile", "Dockerfile")
	req.Header.Set("X-Holodex-Exp", fmt.Sprintf("%d", time.Now().Add(30*time.Minute).Unix()))
	req.Header.Set("X-Holodex-Ticket", job.ticket)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return verifyResult{}, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return verifyResult{}, fmt.Errorf("verify submit %d: %.400s", resp.StatusCode, body)
	}
	var acc struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &acc); err != nil || acc.JobID == "" {
		return verifyResult{}, fmt.Errorf("no holodex job id: %.200s", body)
	}

	deadline := time.Now().Add(18 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		preq, _ := http.NewRequest(http.MethodGet, s.cfg.HolodexURL+"/api/jobs/"+acc.JobID, nil)
		preq.Header.Set("Authorization", "Bearer "+s.cfg.HolodexToken)
		presp, err := client.Do(preq)
		if err != nil {
			continue
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 64<<10))
		presp.Body.Close()
		var out struct {
			State  string       `json:"state"`
			Result verifyResult `json:"result"`
		}
		if err := json.Unmarshal(pbody, &out); err != nil {
			continue
		}
		if out.State == "done" {
			return out.Result, nil
		}
	}
	return verifyResult{}, fmt.Errorf("holodex verification did not finish in time")
}

func writeWorkspaceEnvNote(ws *workspace) {
	// A tiny marker so a confused shell command has an obvious cwd sentinel;
	// purely diagnostic, never read by the agent logic.
	_ = os.WriteFile(ws.root+"/.vela-workspace", []byte(ws.repo+"\n"), 0o644)
}
