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
)

func main() {
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

	agent := NewAgent(cfg)
	bot, err := NewBot(cfg, agent)
	if err != nil {
		log.Fatalf("discord: %v", err)
	}
	if err := bot.Start(); err != nil {
		log.Fatalf("discord connect: %v", err)
	}
	log.Printf("nanoclaw up — model=%s data=%s", cfg.Model, cfg.DataDir)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	bot.Close()
	log.Println("nanoclaw down")
}
