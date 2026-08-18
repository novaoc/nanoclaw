// vela — a pocket AI agent for the LicheeRV Nano.
//
// One static Go binary: Discord gateway in, DeepSeek agent loop with tools
// (web search, page fetch, artifacts, persistent memory), everything stored
// on the microSD. Focused on AI development and agentic engineering — ask it
// for a website mockup, a benchmark rundown, or to think through a build.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Headless eval subcommand: `vela eval <prompts.jsonl> <out.jsonl>`.
	// Drives the real agent loop for load/quality testing; never starts Discord.
	if len(os.Args) >= 4 && os.Args[1] == "eval" {
		runEval(os.Args[2], os.Args[3])
		return
	}
	// `vela pipetest`: one real build request through the whole pipeline,
	// headless — the self-test that replaces "ask in Discord and hope".
	if len(os.Args) >= 2 && os.Args[1] == "pipetest" {
		runPipeTest()
		return
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir+"/artifacts", 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir+"/history", 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	SetupGit(cfg) // authenticate git pushes as Vela when GITHUB_TOKEN is set
	if cfg.CodeEnabled() && cfg.GitHubToken != "" {
		log.Printf("SECURITY: code is enabled with a GITHUB_TOKEN in the environment — a coder shell can " +
			"read it and push anything. Scope the token to blast radius: a fine-grained PAT limited to " +
			"Vela's own repos (contents+PR write, no admin), treated as rotatable.")
	}

	agent := NewAgent(cfg)
	bot, err := NewBot(cfg, agent)
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	agent.SetDiscord(bot) // let tools act in Discord (moderation, forum posts)
	if err := bot.Start(); err != nil {
		log.Fatalf("discord connect: %v", err)
	}
	learnCtx, stopLearning := context.WithCancel(context.Background())
	agent.StartLearning(learnCtx) // background expression learning (2.0)
	log.Printf("vela up — model=%s data=%s talk=%.2f learning=%v", cfg.Model, cfg.DataDir, cfg.TalkValue, cfg.Learning)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	// Graceful drain: a hot-deploy killall must not eat an in-flight turn —
	// finish and reply first (bounded), then drop the gateway. 6 min, not 2:
	// a video generation polls xAI for up to ~5 min, and a 2-min window killed
	// one mid-render. Drain only waits while turns are actually in flight, so
	// the longer ceiling costs nothing on an idle deploy.
	log.Println("vela draining — finishing in-flight turns")
	stopLearning()
	bot.Drain(6 * time.Minute)
	bot.Close()
	log.Println("vela down")
}
