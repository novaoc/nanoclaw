package main

// enqueue_build hands a framed, forked build to the fat-machine worker and
// returns immediately, freeing Vela's turn so she can keep talking. A detached
// poller then narrates the worker's progress into the request thread and — on
// a passing verification — signs and executes the deploy itself, because
// deploy authority never leaves the board.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// A build ticket allows a handful of Holodex verifications and lives long
	// enough for one full build.
	ticketMaxVerifies = 6
	ticketTTL         = 90 * time.Minute
)

func newWorkerJobID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "wj-fallback"
	}
	return "wj-" + hex.EncodeToString(b)
}

// signDeployTicket mirrors holodex deployTicket.canonical exactly.
func signDeployTicket(secret, job, repo string, exp int64) string {
	canonical := strings.Join([]string{
		"holodex-deploy-ticket-v1", job, repo, strconv.FormatInt(exp, 10), "",
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

// signBuildTicket mirrors holodex ticket.canonical exactly.
func signBuildTicket(secret, job, repo string, maxVerifies int, exp int64) string {
	canonical := strings.Join([]string{
		"holodex-ticket-v1", job, repo, strconv.Itoa(maxVerifies), strconv.FormatInt(exp, 10), "",
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

func (tc *ToolCtx) enqueueBuild(a toolArgs) string {
	if !tc.cfg.WorkerEnabled() {
		return "The build worker isn't configured (VELA_WORKER_URL/TOKEN). Use verify_repo/deploy_repo inline instead."
	}
	if g := tc.codeGate(); g != "" {
		return g
	}
	repo := strings.TrimSpace(a.Repo)
	name := strings.TrimSpace(a.Name)
	if repo == "" || name == "" {
		return "enqueue_build needs the repo (owner/name) and the app name."
	}

	gh := newGH(tc.cfg)
	if gh == nil {
		return "GitHub isn't configured, so the worker cannot be pointed at a repo."
	}
	owner, repoName, err := gh.resolveRepo(repo)
	if err != nil {
		return "couldn't resolve " + repo + ": " + err.Error()
	}
	ownerRepo := owner + "/" + repoName
	// Pin the exact commit being shipped. The worker is deterministic and
	// refuses sha-less jobs: verification receipts and deploys are bound to
	// one immutable commit, never to "whatever the branch says later".
	sha, err := gh.headSHA(ownerRepo, "")
	if err != nil {
		return "couldn't resolve the tip of " + ownerRepo + ": " + err.Error() +
			" — has the implementation been pushed?"
	}
	// The worker deploys exactly this commit and writes no code. If the tip
	// is still the scaffold create_rails_app left, nothing has been built —
	// refuse the hand-off instead of shipping an empty app.
	if _, bare := tc.scaffoldTip[sha]; bare {
		return "REFUSED: " + ownerRepo + " is still the untouched scaffold — no implementation has been pushed. Build the product first (put_file/patch_file the models, migrations, controllers, views, tests and styles the request needs, committed to main), then call enqueue_build again. The worker only verifies and deploys what you pushed."
	}

	job := newWorkerJobID()
	exp := time.Now().Add(ticketTTL).Unix()
	ticketSig := signBuildTicket(tc.cfg.SandboxSecret, job, ownerRepo, ticketMaxVerifies, exp)
	ticket := strings.Join([]string{job, ownerRepo, strconv.Itoa(ticketMaxVerifies), strconv.FormatInt(exp, 10), ticketSig}, ":")
	// The deploy grant: single-use, repo-bound, receipt-gated on the server.
	// Signed here — at the moment a human's approval became a build — which
	// is where deploy authority belongs. Expiry is replay protection only, so
	// it is generous enough that a build queued behind others never loses its
	// grant (the 55-minute watcher cliff of the poller era, by design, cannot
	// exist here).
	depExp := time.Now().Add(24 * time.Hour).Unix()
	depSig := signDeployTicket(tc.cfg.SandboxSecret, job, ownerRepo, depExp)
	depTicket := strings.Join([]string{job, ownerRepo, strconv.FormatInt(depExp, 10), depSig}, ":")

	spec := ""
	if tc.appSpec != nil {
		if b, err := MarshalAppSpec(tc.appSpec); err == nil {
			spec = string(b)
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"job_id": job, "repo": ownerRepo, "name": name, "sha": sha, "ticket": ticket,
		"deploy_ticket": depTicket, "channel_id": tc.channelID,
		"spec": spec, "instructions": strings.TrimSpace(a.Instructions), "port": a.Port,
	})

	req, err := http.NewRequest(http.MethodPost, tc.cfg.WorkerURL+"/jobs", bytes.NewReader(payload))
	if err != nil {
		return "enqueue failed: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+tc.cfg.WorkerToken)
	req.Header.Set("Content-Type", "application/json")
	client := *ssrfClient
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return "enqueue failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Sprintf("worker rejected the job (%d): %.300s", resp.StatusCode, body)
	}

	tc.usedCode = true
	// No per-job watcher anymore: the worker owns the whole lifecycle
	// (including the deploy), and the announcer's single change-feed loop
	// posts every transition to the recorded thread — restart-proof, because
	// the mapping is on disk and the truth is on the worker.
	rememberWorkerJob(tc.cfg, job, workerJobNote{Channel: tc.channelID, Name: name, Repo: ownerRepo})
	// Deliberately terminal wording. The first wording ("I'll keep working
	// here") read as *continue* and the model re-ran the entire flow, spec and
	// all, enqueueing the same repo a second time. This says: hand-off done,
	// stop, report.
	return fmt.Sprintf("HAND-OFF COMPLETE — build %q is now the worker's job (%s). Your work on this build is FINISHED: do not call app_spec, create_rails_app, enqueue_build, verify_repo or deploy_repo for it again. Stop using tools and reply to the requester in one short message: the build is queued, progress will be posted in this thread, and the repo and demo links will follow automatically. Then move on to whatever else is asked.", name, job)
}

// pollWorkerJob narrates a worker build and, on success, deploys it. Runs
// detached from the turn that started it.
func (tc *ToolCtx) pollWorkerJob(job, ownerRepo, name string, port int, channelID string, disc Discord) {
	post := func(msg string) {
		if _, err := disc.PostMessage(channelID, msg); err != nil {
			log.Printf("worker poll post: %v", err)
		}
	}
	lastDetail := ""
	deadline := time.Now().Add(55 * time.Minute)

	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Second)
		st, err := tc.workerStatus(job)
		if err != nil {
			continue // transient; the worker is still grinding
		}
		if st.Detail != "" && st.Detail != lastDetail {
			// Only surface meaningful transitions, not every internal tick.
			switch st.State {
			case "verifying", "verified", "failed":
				post("⏳ " + name + ": " + st.Detail)
			}
			lastDetail = st.Detail
		}

		switch st.State {
		case "verified":
			post("✅ " + name + " passed verification. Deploying…")
			tc.deployWorkerResult(st, name, port, post)
			return
		case "failed":
			post("❌ " + name + " didn't make it: " + st.Detail + "\nThe code is on GitHub — I can pick it up from here if you want.")
			return
		}
	}
	post("⌛ " + name + ": the worker ran past its window without finishing. The latest code is on GitHub.")
}

func (tc *ToolCtx) deployWorkerResult(st workerJobStatus, name string, port int, post func(string)) {
	if st.SHA == "" || st.Receipt == "" {
		post("The build verified but the worker didn't return a deploy receipt — I can't deploy this one automatically.")
		return
	}
	deployPort := port
	if deployPort == 0 {
		deployPort = st.Port
	}
	// Deploy is signed on the board with the Holodex secret — the worker
	// never had it. Reference path first, legacy fallback for safety.
	result, supported, err := tc.refDeploy(st.Repo, st.SHA, name, deployPort, st.Receipt)
	if !supported {
		post("The deck's async deploy path wasn't available for this build — tell me to deploy " + st.Repo + "@" + st.SHA[:12] + " and I'll do it inline.")
		return
	}
	if err != nil {
		post("Deploy hit an error: " + err.Error())
		return
	}
	post("🚀 " + renderDeployResult(st.SHA, result))
}

type workerJobStatus struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
	SHA     string `json:"sha"`
	Receipt string `json:"receipt"`
	Port    int    `json:"port"`
}

func (tc *ToolCtx) workerStatus(job string) (workerJobStatus, error) {
	req, err := http.NewRequest(http.MethodGet, tc.cfg.WorkerURL+"/jobs/"+job, nil)
	if err != nil {
		return workerJobStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tc.cfg.WorkerToken)
	client := *ssrfClient
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return workerJobStatus{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode != http.StatusOK {
		return workerJobStatus{}, fmt.Errorf("worker status %d", resp.StatusCode)
	}
	var st workerJobStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return workerJobStatus{}, err
	}
	return st, nil
}
