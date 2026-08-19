package main

// Holodex client: ticket-authorized verification and deployment. The worker
// holds no build secret — its entire authority is the pair of tickets Vela
// signs at enqueue: a budgeted verify ticket and a single-use, receipt-gated
// deploy ticket.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type verifyResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Logs       string `json:"logs"`
	Receipt    string `json:"receipt"`
	Files      int    `json:"files"`
	DurationMS int64  `json:"duration_ms"`
}

// ticketVerify submits a reference verification and polls it to completion.
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

// ticketDeploy ships a verified build using the single-use deploy ticket.
func (s *server) ticketDeploy(job *buildJob, sha, receipt string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, s.cfg.HolodexURL+"/api/deploy/ref", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.HolodexToken)
	req.Header.Set("X-Holodex-Repo", job.Repo)
	req.Header.Set("X-Holodex-Sha", sha)
	req.Header.Set("X-Holodex-Name", job.Name)
	req.Header.Set("X-Holodex-Dockerfile", "Dockerfile")
	req.Header.Set("X-Holodex-Port", fmt.Sprintf("%d", job.Port))
	req.Header.Set("X-Holodex-Exp", fmt.Sprintf("%d", time.Now().Add(30*time.Minute).Unix()))
	req.Header.Set("X-Holodex-Verify", receipt)
	req.Header.Set("X-Holodex-Deploy-Ticket", job.deployTicket)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deploy %d: %.400s", resp.StatusCode, body)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.URL == "" {
		return "", fmt.Errorf("deploy answered without a url: %.200s", body)
	}
	return out.URL, nil
}
