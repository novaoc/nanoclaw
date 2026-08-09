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

// Bankr Agent API client + read/write classification — a copy, so the vault
// is self-contained. The key is per-call; it never leaves this process.

type Bankr struct {
	url    string
	client *http.Client
}

func NewBankr(url string) *Bankr {
	if url == "" {
		url = "https://api.bankr.bot"
	}
	return &Bankr{url: url, client: &http.Client{Timeout: 30 * time.Second}}
}

type bankrJob struct {
	JobID        string `json:"jobId"`
	Status       string `json:"status"`
	Response     string `json:"response"`
	Error        string `json:"error"`
	Transactions []struct {
		Metadata struct {
			HumanReadableMessage string `json:"humanReadableMessage"`
		} `json:"metadata"`
	} `json:"transactions"`
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
		msg := j.Error
		if msg == "" {
			msg = "request failed"
		}
		return &j, fmt.Errorf("bankr %d: %s", resp.StatusCode, msg)
	}
	return &j, nil
}

func (b *Bankr) Prompt(key, text string) (string, error) {
	body, _ := json.Marshal(map[string]string{"prompt": text})
	job, err := b.do(key, "POST", "/agent/prompt", body)
	if err != nil {
		return "", err
	}
	if job.JobID == "" {
		return "", fmt.Errorf("bankr: no job id")
	}
	for i := 0; i < 60; i++ {
		time.Sleep(1500 * time.Millisecond)
		st, err := b.do(key, "GET", "/agent/job/"+job.JobID, nil)
		if err != nil {
			return "", err
		}
		switch st.Status {
		case "completed":
			var sb strings.Builder
			sb.WriteString(strings.TrimSpace(st.Response))
			for _, t := range st.Transactions {
				if m := strings.TrimSpace(t.Metadata.HumanReadableMessage); m != "" {
					sb.WriteString("\n• " + m)
				}
			}
			return strings.TrimSpace(sb.String()), nil
		case "failed", "cancelled":
			return "", fmt.Errorf("bankr job %s: %s", st.Status, st.Error)
		}
	}
	return "", fmt.Errorf("bankr: timed out")
}

// Fail-CLOSED read classifier — matches nanoclaw's; the authoritative copy
// for policy lives HERE, inside the vault.
var readIntent = regexp.MustCompile(`(?i)\b(balance|balances|portfolio|holding|holdings|price|prices|worth|value|fee|fees|address|history|transactions?|status|how\s+much|show|list|view|check)\b`)
var moveVerb = regexp.MustCompile(`(?i)\b(send|transfer|swap|buy|sell|trade|bridge|withdraw|deposit|pay|move|convert|liquidate|approve|claim|launch|deploy|mint|stake|unstake|wrap|unwrap|yeet|ape)\b`)

func IsWalletRead(prompt string) bool {
	return readIntent.MatchString(prompt) && !moveVerb.MatchString(prompt)
}
