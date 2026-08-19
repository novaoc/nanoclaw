package main

// `vela pipetest` — one complete build request through the real pipeline,
// runnable headless on the board over ssh, so the whole road (agent turn →
// enqueue_build → worker → verification → signed deploy → announcement) can
// be tested and re-tested without a human relaying through Discord. Born
// 2026-08-18 after three straight live runs failed in three different ways
// and every diagnosis needed the requester awake.
//
// It never opens a Discord session: a recording stub stands in for the
// channel, and everything Vela would have posted is captured and asserted.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// pipeDisc records what Vela would have posted and feeds it to the waiter.
type pipeDisc struct {
	posts chan string
}

func (d *pipeDisc) PostMessage(channelID, body string) (string, error) {
	fmt.Printf("[post %s] %s\n", time.Now().Format("15:04:05"), body)
	select {
	case d.posts <- body:
	default:
	}
	return "pipetest://" + channelID, nil
}
func (d *pipeDisc) CreateForumPost(forum, title, body string, origin ForumOrigin) (string, string, error) {
	return "pipetest-thread", "pipetest://forum", nil
}
func (d *pipeDisc) Timeout(g, u string, dur time.Duration, r string) error {
	return fmt.Errorf("not in pipetest")
}
func (d *pipeDisc) Kick(g, u, r string) error          { return fmt.Errorf("not in pipetest") }
func (d *pipeDisc) Ban(g, u, r string, days int) error { return fmt.Errorf("not in pipetest") }
func (d *pipeDisc) DeleteMessage(c, m string) error    { return fmt.Errorf("not in pipetest") }
func (d *pipeDisc) Slowmode(c string, s int) error     { return fmt.Errorf("not in pipetest") }
func (d *pipeDisc) ResolveMember(g, q string) (string, string, error) {
	return "", "", fmt.Errorf("not in pipetest")
}
func (d *pipeDisc) ResolveChannel(g, q string) (string, string, int, error) {
	return "pipetest-channel", "pipetest", 0, nil
}

func pipeFail(stage, detail string) {
	fmt.Printf("\nPIPETEST FAIL at %s: %s\n", stage, detail)
	os.Exit(1)
}

func runPipeTest() {
	cfg, err := LoadConfig()
	if err != nil {
		pipeFail("config", err.Error())
	}
	// Isolate all state from the live bot; share only the real services.
	cfg.DataDir = cfg.DataDir + "-pipetest"
	cfg.Workspace = cfg.DataDir + "/workspace"
	_ = os.MkdirAll(cfg.DataDir+"/artifacts", 0o755)
	_ = os.MkdirAll(cfg.DataDir+"/history", 0o755)

	if !cfg.WorkerEnabled() {
		pipeFail("config", "worker is not configured — pipetest exists to test the worker road")
	}
	coder := ""
	for id := range cfg.Coders {
		coder = id
		break
	}
	if coder == "" {
		pipeFail("config", "no coder configured")
	}

	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	appName := "probe-" + hex.EncodeToString(suffix)
	// A mapping left by an earlier pipetest would defeat the exactly-one-job
	// check below; every run starts from a clean slate.
	_ = os.Remove(workerJobsPath(cfg))

	stub := &pipeDisc{posts: make(chan string, 64)}
	agent := NewAgent(cfg)
	agent.SetDiscord(stub)

	content := fmt.Sprintf(
		"Build request, already approved by the requester — start now, do not ask again. "+
			"Build %q: a one-page guestbook where visitors type a short message and see the wall of messages, newest first. "+
			"Keep it as small as the foundation allows.", appName)

	fmt.Printf("PIPETEST start app=%s coder=%s\n", appName, coder)
	turnStart := time.Now()
	reply := agent.HandleTurn(Turn{
		ChannelID: "pipetest", AuthorID: coder, Author: "pipetest",
		Notify:       func(s string) { fmt.Printf("[notify] %s\n", s) },
		SetBuildName: func(s string) {},
	}, content)
	turnDur := time.Since(turnStart).Round(time.Second)
	fmt.Printf("\nSTAGE turn: ended in %s\nreply: %.400s\n", turnDur, reply.Text)

	if strings.Contains(reply.Text, "⚠️") {
		pipeFail("turn", "model error surfaced: "+reply.Text)
	}
	// The turn now legitimately contains the implementation (Vela writes the
	// app; the worker only verifies and deploys), so a long turn is work, not
	// a routing failure. The ceiling only catches a turn that never ends.
	if turnDur > 35*time.Minute {
		pipeFail("turn", fmt.Sprintf("turn took %s — runaway turn", turnDur))
	}

	// In production a Bot goroutine (watchWorkerJobs) turns the worker's
	// change feed into thread posts, and a failed verification triggers one
	// bounded repair turn. Pipetest has no Bot, so it walks the same road by
	// hand: follow the enqueued job; on failure, feed Vela the distilled
	// failure exactly as the announcer would and follow her re-enqueued job.
	jobs := loadWorkerJobs(cfg)
	if len(jobs) != 1 {
		pipeFail("enqueue", fmt.Sprintf("expected exactly 1 remembered worker job, found %d — enqueue_build was not reached (or ran twice)", len(jobs)))
	}
	jobID, repo := "", ""
	for id, n := range jobs {
		jobID, repo = id, n.Repo
	}
	fmt.Printf("STAGE worker: following job %s…\n", jobID)
	state, detail, url := followWorkerJob(cfg, jobID, 40*time.Minute)

	if state == "failed" {
		fmt.Printf("STAGE repair: first verification failed — running the repair turn\n%s\n", detail)
		before := map[string]bool{jobID: true}
		fixStart := time.Now()
		fixReply := agent.HandleTurn(Turn{
			ChannelID: "pipetest", AuthorID: coder, Author: "build-repair",
			Notify:       func(s string) { fmt.Printf("[notify] %s\n", s) },
			SetBuildName: func(s string) {},
		}, repairTurnContent(appName, repo, detail))
		fmt.Printf("STAGE repair turn: ended in %s\nreply: %.300s\n", time.Since(fixStart).Round(time.Second), fixReply.Text)
		jobID = ""
		for id := range loadWorkerJobs(cfg) {
			if !before[id] {
				jobID = id
			}
		}
		if jobID == "" {
			pipeFail("repair", "the repair turn did not re-enqueue a build")
		}
		fmt.Printf("STAGE worker: following repaired job %s…\n", jobID)
		state, detail, url = followWorkerJob(cfg, jobID, 40*time.Minute)
	}

	switch state {
	case "failed":
		pipeFail("worker", detail)
	case "verified":
		pipeFail("deploy", "stopped at verified — deploy ticket missing or deploy failed: "+detail)
	case "deployed":
		// fall through to the URL check below
	default:
		pipeFail("worker", "job ended in unexpected state "+state)
	}
	if url == "" {
		pipeFail("deploy", "deployed without a URL")
	}
	fmt.Printf("STAGE deploy: %s\n", url)
	time.Sleep(10 * time.Second) // container boot grace
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		pipeFail("http", err.Error())
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		pipeFail("http", fmt.Sprintf("GET %s = %d", url, resp.StatusCode))
	}
	fmt.Printf("STAGE http: 200 OK\n\nPIPETEST PASS app=%s total=%s\n",
		appName, time.Since(turnStart).Round(time.Second))
	os.Exit(0)
}

// followWorkerJob polls one worker job until it reaches a terminal state
// (failed | verified | deployed) or the deadline passes.
func followWorkerJob(cfg *Config, jobID string, budget time.Duration) (state, detail, url string) {
	deadline := time.Now().Add(budget)
	lastDetail := ""
	client := &http.Client{Timeout: 45 * time.Second}
	for {
		if time.Now().After(deadline) {
			pipeFail("worker", "job not terminal within "+budget.String())
		}
		time.Sleep(10 * time.Second)
		req, _ := http.NewRequest(http.MethodGet, cfg.WorkerURL+"/jobs/"+jobID, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.WorkerToken)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("[poll] %v\n", err)
			continue
		}
		var st struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
			URL    string `json:"url"`
		}
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if st.Detail != lastDetail {
			fmt.Printf("[job %s] %s | %s\n", time.Now().Format("15:04:05"), st.State, st.Detail)
			lastDetail = st.Detail
		}
		switch st.State {
		case "failed", "verified", "deployed":
			return st.State, st.Detail, st.URL
		}
	}
}
