package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Bankr Agent API client — POST /agent/prompt returns a jobId; poll
// GET /agent/job/{id} until terminal. The Bankr agent owns the wallet,
// execution, and on-chain safety; nanoclaw just relays prompts and reports
// results. See github.com/BankrBot/bankr-api-examples.
//
// The key is per-CALL, not per-client: each user connects their own Bankr
// key (see keystore.go) and their prompts run against their own wallet.

type Bankr struct {
	url    string
	client *http.Client
}

func NewBankr(cfg *Config) *Bankr {
	return &Bankr{url: cfg.BankrURL, client: &http.Client{Timeout: 30 * time.Second}}
}

type bankrJob struct {
	Success      bool   `json:"success"`
	JobID        string `json:"jobId"`
	Status       string `json:"status"`
	Response     string `json:"response"`
	Error        string `json:"error"`
	Transactions []struct {
		Metadata struct {
			HumanReadableMessage string `json:"humanReadableMessage"`
		} `json:"metadata"`
	} `json:"transactions"`
	StatusUpdates []struct {
		Message string `json:"message"`
	} `json:"statusUpdates"`
}

func (b *Bankr) do(key, method, path string, body []byte) (*bankrJob, error) {
	req, err := http.NewRequest(method, b.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var j bankrJob
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, fmt.Errorf("bankr %d: bad response", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return &j, fmt.Errorf("bankr %d: %s", resp.StatusCode, orDefault(j.Error, "request failed"))
	}
	return &j, nil
}

// Prompt submits a natural-language instruction to the Bankr agent using
// the given user's key, and polls to completion, returning the agent's
// final response plus any transaction summaries.
func (b *Bankr) Prompt(key, text string) (string, error) {
	body, _ := json.Marshal(map[string]string{"prompt": text})
	job, err := b.do(key, "POST", "/agent/prompt", body)
	if err != nil {
		return "", err
	}
	if job.JobID == "" {
		return "", fmt.Errorf("bankr: no job id")
	}
	for i := 0; i < 60; i++ { // ~90s at 1.5s
		time.Sleep(1500 * time.Millisecond)
		st, err := b.do(key, "GET", "/agent/job/"+job.JobID, nil)
		if err != nil {
			return "", err
		}
		switch st.Status {
		case "completed":
			return formatJob(st), nil
		case "failed", "cancelled":
			return "", fmt.Errorf("bankr job %s: %s", st.Status, orDefault(st.Error, ""))
		}
	}
	return "", fmt.Errorf("bankr: timed out waiting for the job")
}

func formatJob(j *bankrJob) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(j.Response))
	for _, t := range j.Transactions {
		if m := strings.TrimSpace(t.Metadata.HumanReadableMessage); m != "" {
			b.WriteString("\n• " + m)
		}
	}
	return strings.TrimSpace(b.String())
}

// isWalletRead is a fail-CLOSED allowlist: only prompts that clearly just
// look something up execute without confirmation. A verb DENYlist would miss
// "yeet my ETH to 0x…", "move", "pay", "convert" — so anything not matching
// here is treated as fund-moving and routed to the out-of-band confirmation.
var readIntent = regexp.MustCompile(`(?i)\b(balance|balances|portfolio|holding|holdings|price|prices|worth|value|fee|fees|address|history|transactions?|status|how\s+much|show|list|view|check)\b`)

// a read phrasing that ALSO carries a move verb is NOT a read.
var moveVerb = regexp.MustCompile(`(?i)\b(send|transfer|swap|buy|sell|trade|bridge|withdraw|deposit|pay|move|convert|liquidate|approve|claim|launch|deploy|mint|stake|unstake|wrap|unwrap|yeet|ape)\b`)

func isWalletRead(prompt string) bool {
	return readIntent.MatchString(prompt) && !moveVerb.MatchString(prompt)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
