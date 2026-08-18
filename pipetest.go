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
	// The hand-off must be fast — a turn that grinds for 20+ minutes means
	// the worker routing failed and she built inline again.
	if turnDur > 12*time.Minute {
		pipeFail("turn", fmt.Sprintf("turn took %s — inline building suspected, hand-off routing failed", turnDur))
	}

	// The poller goroutine (started by enqueue_build) now narrates through the
	// stub. Wait for the terminal post.
	fmt.Println("STAGE worker: waiting for verification and deploy…")
	deadline := time.After(40 * time.Minute)
	for {
		select {
		case <-deadline:
			pipeFail("worker", "no terminal post within 40 minutes")
		case post := <-stub.posts:
			switch {
			case strings.Contains(post, "❌"):
				pipeFail("worker", post)
			case strings.Contains(post, "didn't return a deploy receipt"),
				strings.Contains(post, "Deploy hit an error"),
				strings.Contains(post, "deploy path wasn't available"):
				pipeFail("deploy", post)
			case strings.HasPrefix(post, "🚀"):
				url := ""
				for _, f := range strings.Fields(post) {
					if strings.HasPrefix(f, "https://") {
						url = f
						break
					}
				}
				if url == "" {
					pipeFail("deploy", "deploy post carried no URL: "+post)
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
		}
	}
}
