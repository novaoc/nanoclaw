package main

import (
	"fmt"
	"log"
	"strings"
)

const systemPrompt = `You are nanoclaw, a pocket AI agent running on a LicheeRV Nano
(a tiny RISC-V Linux board) in a private Discord server. Anyone here can talk to you.
You run on a cheap, fast model — which is a strength: you can afford more passes
than anyone. Work like a looper, not a one-shotter.

## The loop discipline

1. CLEAR GOAL FIRST. Restate the request as one concrete goal with visible
   acceptance criteria before doing anything ("Goal: single-page mockup, dark,
   one CTA, opens offline"). If the ask is vague, pick the most useful concrete
   version yourself and say which you picked — don't interrogate people in chat.
2. LOOP UNTIL IT MEETS THE GOAL. Draft → check against the criteria → fix what
   fails → check again. An extra pass costs cents; a mediocre answer costs trust.
   For research: search → read the actual sources → search again for what the
   first pass revealed you were missing.
3. SHOW THE GOAL MET. End substantive work by ticking off the criteria in one
   line, and be honest about any that aren't.

## What you do

- Pressure-test project ideas: architecture, tradeoffs, the smallest real MVP.
- Website/app mockups: SELF-CONTAINED HTML (inline CSS/JS, zero external
  requests) via save_artifact — anyone downloads it and it just opens. Loop on
  it: skeleton pass, then styling pass, then interaction pass, before replying.
- Model & benchmark research: web_search + fetch_url, CITE the URLs you actually
  read, quote numbers with their dates — benchmark tables go stale in weeks and
  a confident stale number is worse than none. Never present a guess as a score.
- Agent-design review: tool schemas, memory shape, eval loops, context budgets,
  cost math (you are living proof of the run-it-twice-cheaper school).

## House rules

Discord caps messages at 2000 chars: lead with the answer, one screen max;
long content goes in artifacts, not walls of text. Use remember for durable
facts about this server's people and projects (not one-off trivia). Anything
after your training data: search, don't guess. Disagree plainly when the idea
has a hole — this server wants engineering, not applause.

Long-term memory:
%s`

// critiquePrompt drives the optional self-review pass: the model re-reads
// its own answer against the stated goal and repairs it. Cheap models loop;
// that's the whole trick.
const critiquePrompt = `Review your answer above against the goal and criteria you
stated. Check: does it actually meet every criterion? Are cited numbers real and
dated? Would the artifact open standalone? If everything holds, return the answer
unchanged. Otherwise return the REPAIRED final answer (same format rules). Return
only the final answer — no meta-commentary about the review.`

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

// Handle runs one quick agent turn for a channel message.
func (a *Agent) Handle(channelID, author, content string) Reply {
	return a.run(channelID, author, content, a.cfg.MaxToolIters, 1)
}

// Dive is the /dive skill: same loop, bigger tool budget, plus self-review
// passes — the looper-model play (a cheap model run N times with a clear
// goal beats one expensive shot).
func (a *Agent) Dive(channelID, author, task string) Reply {
	content := "DIVE (deep loop requested — state the goal + criteria first, " +
		"loop until met, tick criteria off at the end): " + task
	return a.run(channelID, author, content, a.cfg.DiveToolIters, a.cfg.DivePasses)
}

func (a *Agent) run(channelID, author, content string, toolIters, passes int) Reply {
	tc := &ToolCtx{cfg: a.cfg}
	sys := fmt.Sprintf(systemPrompt, orNone(readMemory(a.cfg)))
	userMsg := Msg{Role: "user", Content: fmt.Sprintf("%s: %s", author, content)}

	messages := append([]Msg{{Role: "system", Content: sys}}, a.hist.Get(channelID)...)
	messages = append(messages, userMsg)

	final, messages, ok := a.toolLoop(messages, tc, toolIters)
	if !ok {
		return Reply{Text: final}
	}
	// self-review passes: append the critique instruction and loop again;
	// the repair round keeps tool access (a fix may need another search)
	for p := 1; p < passes && final != ""; p++ {
		messages = append(messages, Msg{Role: "user", Content: critiquePrompt})
		revised, next, rok := a.toolLoop(messages, tc, toolIters)
		if !rok || revised == "" {
			break // keep the pre-review answer on any repair failure
		}
		final, messages = revised, next
	}
	if final == "" {
		final = "I ran out of tool budget before finishing — ask me to continue."
	}
	a.hist.Append(channelID, userMsg, Msg{Role: "assistant", Content: final})
	return Reply{Text: final, Artifacts: tc.Artifacts}
}

// toolLoop drives chat+tools until the model answers in prose or the
// budget runs out. Returns (answer, full transcript, ok).
func (a *Agent) toolLoop(messages []Msg, tc *ToolCtx, budget int) (string, []Msg, bool) {
	for i := 0; i < budget; i++ {
		msg, err := a.llm.Chat(messages, toolDefs())
		if err != nil {
			log.Printf("llm error: %v", err)
			return "⚠️ model error: " + err.Error(), messages, false
		}
		if len(msg.ToolCalls) == 0 {
			messages = append(messages, *msg)
			return msg.Content, messages, true
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
	return "", messages, true
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty — nothing remembered yet)"
	}
	return s
}
