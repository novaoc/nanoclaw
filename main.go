// nanoclaw — a MimiClaw-style pocket agent for the LicheeRV Nano.
//
// One static Go binary: Discord gateway in, DeepSeek agent loop with tools
// (web search, page fetch, artifacts, persistent memory), everything stored
// on the microSD. Focused on AI development and agentic engineering — ask it
// for a website mockup, a benchmark rundown, or to think through a build.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Headless eval subcommand: `nanoclaw eval <prompts.jsonl> <out.jsonl>`.
	// Drives the real agent loop for load/quality testing; never starts Discord.
	if len(os.Args) >= 4 && os.Args[1] == "eval" {
		runEval(os.Args[2], os.Args[3])
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
	log.Printf("nanoclaw up — model=%s data=%s", cfg.Model, cfg.DataDir)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	// Graceful drain: a hot-deploy killall must not eat an in-flight turn —
	// finish and reply first (bounded), then drop the gateway.
	log.Println("nanoclaw draining — finishing in-flight turns")
	bot.Drain(2 * time.Minute)
	bot.Close()
	log.Println("nanoclaw down")
}
