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

type Bankr struct {
	url    string
	key    string
	client *http.Client
}

func NewBankr(cfg *Config) *Bankr {
	if cfg.BankrKey == "" {
		return nil
	}
	return &Bankr{url: cfg.BankrURL, key: cfg.BankrKey, client: &http.Client{Timeout: 30 * time.Second}}
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

func (b *Bankr) do(method, path string, body []byte) (*bankrJob, error) {
	req, err := http.NewRequest(method, b.url+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", b.key)
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

// Prompt submits a natural-language instruction to the Bankr agent and
// polls to completion, returning the agent's final response plus any
// transaction summaries.
func (b *Bankr) Prompt(text string) (string, error) {
	body, _ := json.Marshal(map[string]string{"prompt": text})
	job, err := b.do("POST", "/agent/prompt", body)
	if err != nil {
		return "", err
	}
	if job.JobID == "" {
		return "", fmt.Errorf("bankr: no job id")
	}
	for i := 0; i < 60; i++ { // ~90s at 1.5s
		time.Sleep(1500 * time.Millisecond)
		st, err := b.do("GET", "/agent/job/"+job.JobID, nil)
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

// writeIntent flags prompts that would move funds or deploy — those need an
// admin + confirmation. Reads (balances, prices, fees, portfolio) don't.
var writeVerb = regexp.MustCompile(`(?i)\b(send|transfer|swap|buy|sell|trade|bridge|withdraw|deposit|approve|claim|launch|deploy|mint|stake|unstake|wrap|unwrap|create\s+(a\s+)?wallet|new\s+wallet|login|register)\b`)

func isBankrWrite(prompt string) bool { return writeVerb.MatchString(prompt) }

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
