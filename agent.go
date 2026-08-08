package main

import (
	"fmt"
	"log"
	"strings"
)

const systemPrompt = `You are nanoclaw, a pocket AI agent running on a LicheeRV Nano
(a tiny RISC-V Linux board) in a private Discord server. Anyone here can talk to you.

Your focus: AI development and agentic engineering. You help with:
- exploring and pressure-testing project ideas (architecture, tradeoffs, MVPs)
- website/app mockups — build them as SELF-CONTAINED HTML files (inline CSS/JS,
  no external requests) via save_artifact, so anyone can download and open them
- researching models, benchmarks, papers, and agent frameworks — use web_search
  and fetch_url, and CITE the URLs you actually read
- reviewing agent designs: tool schemas, memory, eval loops, context budgets

Style: direct, technical, concise. Discord messages cap at 2000 chars — lead with
the answer; put long content in artifacts instead of walls of text. Use remember
for durable facts about this server's people and projects. If a question needs
current data (anything after your training data), search rather than guess.

Long-term memory:
%s`

type Agent struct {
	cfg  *Config
	llm  *LLM
	hist *History
}

type Reply struct {
	Text      string
	Artifacts []string
}

func NewAgent(cfg *Config) *Agent {
	return &Agent{cfg: cfg, llm: NewLLM(cfg), hist: NewHistory(cfg)}
}

// Handle runs one full agent turn for a channel message.
func (a *Agent) Handle(channelID, author, content string) Reply {
	tc := &ToolCtx{cfg: a.cfg}
	sys := fmt.Sprintf(systemPrompt, orNone(readMemory(a.cfg)))
	userMsg := Msg{Role: "user", Content: fmt.Sprintf("%s: %s", author, content)}

	messages := append([]Msg{{Role: "system", Content: sys}}, a.hist.Get(channelID)...)
	messages = append(messages, userMsg)

	var final string
	for i := 0; i < a.cfg.MaxToolIters; i++ {
		msg, err := a.llm.Chat(messages, toolDefs())
		if err != nil {
			log.Printf("llm error: %v", err)
			return Reply{Text: "⚠️ model error: " + err.Error()}
		}
		if len(msg.ToolCalls) == 0 {
			final = msg.Content
			break
		}
		messages = append(messages, *msg)
		for _, call := range msg.ToolCalls {
			log.Printf("tool %s(%.120s)", call.Function.Name, call.Function.Arguments)
			result := tc.Run(call.Function.Name, call.Function.Arguments)
			if len(result) > 8000 {
				result = result[:8000] + " …[truncated]"
			}
			messages = append(messages, Msg{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	if final == "" {
		final = "I ran out of tool budget before finishing — ask me to continue."
	}
	a.hist.Append(channelID, userMsg, Msg{Role: "assistant", Content: final})
	return Reply{Text: final, Artifacts: tc.Artifacts}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty — nothing remembered yet)"
	}
	return s
}
