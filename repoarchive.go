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
	"unicode/utf8"
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
		"holodex-archive-v1",
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
	// Reference path first: Holodex fetches the public archive for the SHA
	// itself and verifies it as a polled job, so the tarball never crosses
	// this board and no long HTTP connection is held open.
	if gh := newGH(tc.cfg); gh != nil {
		ownerRepo, sha, public, err := resolveCommit(gh, a.Repo, strings.TrimSpace(a.Ref))
		if err == nil && public {
			result, supported, rerr := tc.refVerify(ownerRepo, sha, name, target, dockerfile)
			if supported {
				if rerr != nil {
					return "Holodex reference verification failed: " + rerr.Error()
				}
				return renderVerifyResult(a.Repo, sha, result)
			}
			// Old server without the job API — fall through to the upload path.
		}
	}

	p := repoArchiveParams{Action: "verify", Name: name, Target: target, Dockerfile: dockerfile}
	result, sha := tc.sendRepoArchive(a.Repo, a.Ref, "/api/verify", p, "")
	if sha == "" {
		return result
	}
	return renderVerifyResult(a.Repo, sha, result)
}

// renderVerifyResult formats a verification outcome (the JSON body shared by
// the sync endpoint and the job API) for the model.
func renderVerifyResult(repo, sha, result string) string {
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
		return fmt.Sprintf("Verification FAILED for %s@%s: %s\n%s", repo, sha, out.Error, failureExcerpt(out.Logs, 6000))
	}
	logTail := clip(out.Logs, 3500)
	return fmt.Sprintf("Verification PASSED for %s@%s (%d files, %.1fs). Use deploy_repo with ref=%s and receipt=%s to deploy this exact tested source.\n%s",
		repo, sha, out.Files, float64(out.DurationMS)/1000, sha, out.Receipt, logTail)
}

func (tc *ToolCtx) deployRepo(a toolArgs) string {
	if g := tc.repoBuildGate(a.Repo); g != "" {
		return g
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Ref) == "" || strings.TrimSpace(a.Receipt) == "" {
		return "deploy_repo needs name, the exact verified ref SHA, and the receipt returned by verify_repo."
	}

	// Reference path: the receipt-bound archive is already retained on the
	// server from the reference verification — deploy it without re-uploading.
	ref := strings.ToLower(strings.TrimSpace(a.Ref))
	if len(ref) == 40 {
		if gh := newGH(tc.cfg); gh != nil {
			if owner, name, err := gh.resolveRepo(a.Repo); err == nil {
				result, supported, rerr := tc.refDeploy(owner+"/"+name, ref, strings.TrimSpace(a.Name), a.Port, strings.TrimSpace(a.Receipt))
				if supported {
					if rerr != nil {
						return "Holodex reference deploy failed: " + rerr.Error()
					}
					return renderDeployResult(ref, result)
				}
				// Old server or a legacy-upload receipt — fall through.
			}
		}
	}

	p := repoArchiveParams{Action: "deploy", Name: strings.TrimSpace(a.Name), Dockerfile: "Dockerfile", Port: a.Port}
	result, sha := tc.sendRepoArchive(a.Repo, a.Ref, "/api/deploy/archive", p, strings.TrimSpace(a.Receipt))
	if sha == "" {
		return result
	}
	return renderDeployResult(sha, result)
}

// renderDeployResult formats a deploy outcome (shared JSON body) for the model.
func renderDeployResult(sha, result string) string {
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

// failureExcerpt distills a failed verification log to what the model can act
// on: the test-failure paragraphs (minitest prints them as "Error:"/"Failure:"
// blocks) plus the tail of the log, where the build step actually died. The
// head is deliberately dropped — a Rails image build opens with hundreds of
// lines of package-install noise, and clipping from the front used to hide
// every real error past the 6 KB mark, so the model iterated blind.
func failureExcerpt(logs string, budget int) string {
	if len(logs) <= budget {
		return logs
	}
	norm := strings.ReplaceAll(logs, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	lines := strings.Split(norm, "\n")

	var blocks []string
	seen := map[string]bool{} // BuildKit prints the test output twice (stream + final error dump)
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(stripBuildPrefix(lines[i]))
		if t != "Error:" && t != "Failure:" {
			continue
		}
		var block []string
		for j := i; j < len(lines) && j < i+10; j++ {
			s := stripBuildPrefix(lines[j])
			if j > i && strings.TrimSpace(s) == "" {
				break
			}
			block = append(block, s)
		}
		i += len(block) - 1
		b := strings.Join(block, "\n")
		if !seen[b] {
			seen[b] = true
			blocks = append(blocks, b)
		}
	}

	head := ""
	if len(blocks) > 0 {
		head = "Test failures:\n" + strings.Join(blocks, "\n\n")
		if len(head) > budget/2 {
			head = clip(head, budget/2)
		}
		head += "\n\n"
	}

	tail := norm
	if tailBudget := budget - len(head); len(tail) > tailBudget {
		cut := len(tail) - tailBudget
		for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
			cut++
		}
		tail = "…[log tail]\n" + tail[cut:]
	}
	return head + tail
}

// stripBuildPrefix removes BuildKit's "#12 34.56 " line prefix so minitest
// output inside a docker build log can be recognised and read.
func stripBuildPrefix(line string) string {
	rest, ok := strings.CutPrefix(line, "#")
	if !ok {
		return line
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(rest) || rest[i] != ' ' {
		return line
	}
	rest = rest[i+1:]
	// optional elapsed-seconds column ("51.99 ")
	j := 0
	for j < len(rest) && (rest[j] >= '0' && rest[j] <= '9' || rest[j] == '.') {
		j++
	}
	if j > 0 && j < len(rest) && rest[j] == ' ' {
		return rest[j+1:]
	}
	return rest
}

func (tc *ToolCtx) repoBuildGate(repo string) string {
	if tc.cfg.SandboxURL == "" || tc.cfg.SandboxToken == "" || tc.cfg.SandboxSecret == "" {
		return "Holodex repository builds aren't fully configured (VELA_SANDBOX_URL/TOKEN/SECRET)."
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
// token, then streams the archive to Holodex. The token never leaves the Nano;
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
	req.Header.Set("X-Holodex-Sign", sig)
	req.Header.Set("X-Holodex-Name", p.Name)
	req.Header.Set("X-Holodex-Target", p.Target)
	req.Header.Set("X-Holodex-Dockerfile", p.Dockerfile)
	if p.Port != 0 {
		req.Header.Set("X-Holodex-Port", strconv.Itoa(p.Port))
	}
	if receipt != "" {
		req.Header.Set("X-Holodex-Verify", receipt)
	}
	client := *ssrfClient
	client.Timeout = 20 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return "Holodex request failed: " + err.Error(), ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return holodexFailureMessage(resp.StatusCode, repo, sha, raw), ""
	}
	return string(raw), sha
}

// holodexFailureMessage renders an HTTP-error response from Holodex for the
// model. A failed verification arrives as a 422 whose JSON body embeds the
// whole build log — dumping the raw JSON head gave the model kilobytes of
// package-install noise and hid the actual error past the clip, so it
// iterated blind (2026-08-14: eight blind verifies on one missing helper).
// Distill the embedded log the same way the ok=false path does.
func holodexFailureMessage(status int, repo, sha string, raw []byte) string {
	var out struct {
		Error string `json:"error"`
		Logs  string `json:"logs"`
	}
	if json.Unmarshal(raw, &out) == nil && strings.TrimSpace(out.Logs) != "" {
		return fmt.Sprintf("Holodex %d for %s@%s: %s\n%s", status, repo, sha, out.Error, failureExcerpt(out.Logs, 6000))
	}
	return fmt.Sprintf("Holodex %d for %s@%s: %.12000s", status, repo, sha, raw)
}
