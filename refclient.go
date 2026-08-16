package main

// Client half of Holodex's reference/async protocol (see holodex jobs.go).
//
// Instead of downloading a repository tarball onto this 256 MB board and
// streaming it back out while holding one HTTP connection for the whole
// Docker build, verify_repo sends a signed {repo, sha} reference; Holodex
// fetches the public archive itself and verifies it as a job this client
// polls. Deploys reference the retained, receipt-bound archive — the token
// and tarball never cross the board at all.
//
// Fallbacks keep every old path alive: an old server (404 on the ref
// endpoints) or a private repository (Holodex has no GitHub credentials, by
// design) falls back to the legacy signed-upload flow transparently.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	refSigTTL     = 15 * time.Minute
	refPollEvery  = 5 * time.Second
	refPollBudget = 16 * time.Minute // just past the server's verify timeout
)

type refRequest struct {
	Action     string
	Repo       string // owner/name
	SHA        string
	Name       string
	Target     string
	Dockerfile string
	Port       int
	Exp        int64
}

// canonicalRef mirrors holodex's refParams.canonical byte for byte.
func (p refRequest) canonical() string {
	return strings.Join([]string{
		"holodex-ref-v1", p.Action, p.Repo, p.SHA, p.Name, p.Target,
		p.Dockerfile, strconv.Itoa(p.Port), strconv.FormatInt(p.Exp, 10), "",
	}, "\n")
}

func (p refRequest) headers(secret string) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, p.canonical())
	return map[string]string{
		"X-Holodex-Repo":       p.Repo,
		"X-Holodex-Sha":        p.SHA,
		"X-Holodex-Name":       p.Name,
		"X-Holodex-Target":     p.Target,
		"X-Holodex-Dockerfile": p.Dockerfile,
		"X-Holodex-Port":       strconv.Itoa(p.Port),
		"X-Holodex-Exp":        strconv.FormatInt(p.Exp, 10),
		"X-Holodex-Sign":       hex.EncodeToString(mac.Sum(nil)),
	}
}

// resolveCommit turns repo+ref into (owner/name, full sha, isPublic) with two
// REST calls and no archive download.
func resolveCommit(g *ghClient, repo, ref string) (string, string, bool, error) {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "", "", false, err
	}
	info, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", owner, name), nil)
	if err != nil {
		return "", "", false, err
	}
	if st >= 400 {
		return "", "", false, fmt.Errorf("%s", ghErr(info, st))
	}
	private, _ := info["private"].(bool)
	if strings.TrimSpace(ref) == "" {
		ref, _ = info["default_branch"].(string)
		if ref == "" {
			ref = "main"
		}
	}
	commit, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/commits/%s", owner, name, ref), nil)
	if err != nil {
		return "", "", false, err
	}
	if st >= 400 {
		return "", "", false, fmt.Errorf("%s", ghErr(commit, st))
	}
	sha, _ := commit["sha"].(string)
	if len(sha) != 40 {
		return "", "", false, fmt.Errorf("GitHub did not resolve %s to a commit", ref)
	}
	return owner + "/" + name, sha, !private, nil
}

func (tc *ToolCtx) refPost(path string, p refRequest, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, tc.cfg.SandboxURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tc.cfg.SandboxToken)
	for k, v := range p.headers(tc.cfg.SandboxSecret) {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	client := *ssrfClient
	client.Timeout = 2 * time.Minute
	return client.Do(req)
}

// refVerify runs the async reference verification end to end: submit, poll,
// return the same result JSON the sync endpoint produces.
// supported=false means the server predates the job API — use the legacy path.
func (tc *ToolCtx) refVerify(ownerRepo, sha, name, target, dockerfile string) (result string, supported bool, err error) {
	p := refRequest{
		Action: "verify", Repo: ownerRepo, SHA: sha, Name: name,
		Target: target, Dockerfile: dockerfile,
		Exp: time.Now().Add(refSigTTL).Unix(),
	}
	resp, err := tc.refPost("/api/verify/ref", p, nil)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusAccepted {
		return "", true, fmt.Errorf("Holodex %d: %.2000s", resp.StatusCode, raw)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if json.Unmarshal(raw, &accepted) != nil || accepted.JobID == "" {
		return "", true, fmt.Errorf("Holodex sent no job id: %.500s", raw)
	}

	deadline := time.Now().Add(refPollBudget)
	lastNotify := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(refPollEvery)
		req, err := http.NewRequest(http.MethodGet, tc.cfg.SandboxURL+"/api/jobs/"+accepted.JobID, nil)
		if err != nil {
			return "", true, err
		}
		req.Header.Set("Authorization", "Bearer "+tc.cfg.SandboxToken)
		client := *ssrfClient
		client.Timeout = 30 * time.Second
		pollResp, err := client.Do(req)
		if err != nil {
			continue // transient poll failure — the job is still running server-side
		}
		body, _ := io.ReadAll(io.LimitReader(pollResp.Body, 64<<10))
		pollResp.Body.Close()
		if pollResp.StatusCode != http.StatusOK {
			return "", true, fmt.Errorf("job poll %d: %.500s", pollResp.StatusCode, body)
		}
		var out struct {
			State  string          `json:"state"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", true, err
		}
		if out.State == "done" {
			return string(out.Result), true, nil
		}
		if time.Since(lastNotify) > 90*time.Second && tc.notify != nil {
			tc.notify(fmt.Sprintf("⏳ Holodex is still verifying %s@%.12s…", ownerRepo, sha))
			lastNotify = time.Now()
		}
	}
	return "", true, fmt.Errorf("verification job %s did not finish within %s", accepted.JobID, refPollBudget)
}

// refDeploy deploys a previously reference-verified commit using its receipt.
// supported=false → legacy path (old server, or no retained archive).
func (tc *ToolCtx) refDeploy(ownerRepo, sha, name string, port int, receipt string) (result string, supported bool, err error) {
	p := refRequest{
		Action: "deploy", Repo: ownerRepo, SHA: sha, Name: name,
		Dockerfile: "Dockerfile", Port: port,
		Exp: time.Now().Add(refSigTTL).Unix(),
	}
	resp, err := tc.refPost("/api/deploy/ref", p, map[string]string{"X-Holodex-Verify": receipt})
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", false, nil
	case resp.StatusCode == http.StatusPreconditionFailed && strings.Contains(string(raw), "no verified archive retained"):
		// Receipt may have come from a legacy byte-upload verify — fall back.
		return "", false, nil
	case resp.StatusCode >= 400:
		return "", true, fmt.Errorf("Holodex %d: %.2000s", resp.StatusCode, raw)
	}
	return string(raw), true, nil
}
