package main

// Minimal OpenAI-compatible tool-calling loop for the worker. Deliberately
// standalone (no import of the board's agent) — the worker's belt, prompt,
// and failure modes are its own, and the two evolve independently.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type chatMsg struct {
	Role string `json:"role"`
	// No omitempty: the provider returns assistant messages with tool_calls
	// and an EMPTY content, and several OpenAI-compatible backends reject a
	// replayed assistant message that has no content field at all. Vela's
	// own client always sends it too.
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func def(name, description, params string) toolDef {
	var d toolDef
	d.Type = "function"
	d.Function.Name = name
	d.Function.Description = description
	d.Function.Parameters = json.RawMessage(params)
	return d
}

const (
	// Bounded generation. Without a cap the upstream will happily run far
	// past anything useful; the largest legitimate reply here is one whole
	// source file, which fits comfortably.
	chatMaxTokens = 8192
	// One attempt's ceiling. The aggregator fans out to several backends
	// (DeepInfra, Novita, …) and a slow one used to stall an entire build:
	// a 5-minute timeout × 4 retries meant 20 minutes of nothing. Cutting a
	// stuck attempt early and re-rolling usually lands on a healthy backend.
	chatAttemptTimeout = 90 * time.Second
	chatAttempts       = 5
)

// chat calls the model once, with retries on transient failures.
func (s *server) chat(messages []chatMsg, tools []toolDef) (chatMsg, error) {
	payload, err := json.Marshal(map[string]any{
		"model":      s.cfg.Model,
		"messages":   messages,
		"tools":      tools,
		"max_tokens": chatMaxTokens,
	})
	if err != nil {
		return chatMsg{}, err
	}

	var lastErr error
	for attempt := 0; attempt < chatAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 3 * time.Second)
		}
		// A fresh reader per attempt: the previous one is consumed, and a
		// retry with a spent body would post an empty request.
		req, err := http.NewRequest(http.MethodPost, s.cfg.ModelURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return chatMsg{}, err
		}
		req.Header.Set("Authorization", "Bearer "+s.cfg.ModelKey)
		req.Header.Set("Content-Type", "application/json")
		// The Nous endpoint sits behind Cloudflare, which rejects the default
		// Go UA; a browser-ish UA passes (same trick the board uses).
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; vela-worker/1.0)")

		client := &http.Client{Timeout: chatAttemptTimeout}
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("model attempt %d failed after %s: %v", attempt+1, time.Since(started).Round(time.Second), err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("model %d: %.300s", resp.StatusCode, body)
			continue
		}
		if resp.StatusCode != 200 {
			return chatMsg{}, fmt.Errorf("model %d: %.800s", resp.StatusCode, body)
		}
		var out struct {
			Choices []struct {
				Message      chatMsg `json:"message"`
				FinishReason string  `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
			lastErr = fmt.Errorf("unparseable model response: %.300s", body)
			log.Printf("model attempt %d unparseable after %s", attempt+1, time.Since(started).Round(time.Second))
			continue
		}
		msg := out.Choices[0].Message
		// An empty completion (no content, no tool calls — usually
		// finish=length after a long hidden reasoning run) is a wasted turn,
		// and surfacing it to the loop used to burn an iteration plus a nudge
		// message of context every time. Treat it like a transient failure
		// and re-roll; the aggregator rarely produces two in a row.
		if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
			lastErr = fmt.Errorf("empty completion (finish=%s)", out.Choices[0].FinishReason)
			log.Printf("model attempt %d empty (finish=%s) after %s — retrying", attempt+1, out.Choices[0].FinishReason, time.Since(started).Round(time.Second))
			continue
		}
		log.Printf("model ok in %s (finish=%s, tools=%d)",
			time.Since(started).Round(time.Second), out.Choices[0].FinishReason, len(msg.ToolCalls))
		return msg, nil
	}
	return chatMsg{}, lastErr
}

const workerSystemPrompt = `You are the build worker for Vela's app pipeline. You receive a freshly
forked Rails application (the Vela foundation) plus a product specification,
and your job is to implement the product, get the test suite green, push, and
pass Holodex verification. You work alone; nobody answers questions. Budget
your verifications — each costs minutes and the ticket allows only a few.

THE LOOP THAT WORKS:
1. Look around — BRIEFLY. list_tree, read docs/APP_SPEC.json, read
   config/routes.rb, and read ONE neighbouring test. That is enough; you are
   not required to understand the whole foundation before you touch it, and
   reading more than about a dozen files before your first write is a failure
   mode, not diligence. run_tests will teach you the rest in seconds.
2. Implement in small steps with write_file. The foundation's conventions:
   - Integration tests define their own sign-in helper (there is NO
     Devise::Test::IntegrationHelpers anywhere):
       PASSWORD = "correct horse battery" # matches test/fixtures/users.yml
       def sign_in_as(user, password: PASSWORD)
         post user_session_path, params: { user: { email: user.email, password: password } }
       end
   - Before writing any test, read a neighboring test in the same directory
     and copy its conventions.
   - Never edit Gemfile.lock, bin/brakeman, or material_tokens.css.
   - Replace every template identity ("Application", example.com) with the
     product's own; the foundation config test asserts the stamped identity.
2b. EXPECT A RED SUITE ON ARRIVAL. The fork is identity-stamped (a real
   product domain in config/foundation.yml) but a handful of template tests
   still assert the old example.com identity — billing, checkout services,
   receipt dispatch, runtime config. Your first fix is aligning those tests
   with the stamped identity. That is expected, designed work; do it before
   anything else and the suite baseline goes green.
3. run_tests EARLY and often — it is fast and local. Fix what it names.
   Also run "bin/rubocop" and "bin/brakeman --quiet --no-pager --exit-on-warn"
   via shell before verifying; verification gates on both.
4. When local checks are green: commit_and_push, then verify. If verification
   fails, the excerpt names the failing tests — fix exactly those.
5. When a verification PASSES you are done: call done with a short summary.
   Do not keep polishing after a pass.

Never invent your own sign-off criteria: green local suite → push → verify →
done. If you cannot get green within your budget, call done with what you
learned — an honest failure report beats a burned ticket.`

// agentTools is the worker belt schema, OpenAI function format.
func agentTools() []toolDef {
	return []toolDef{
		def("list_tree", "List the repository file tree (truncated at 400 entries).", `{"type":"object","properties":{}}`),
		def("read_file", "Read one file from the workspace.", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		def("write_file", "Create or fully replace one file in the workspace. Send the COMPLETE file content.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		def("shell", "Run a bash command inside the workspace (max 10 minutes). Use for rg/grep, rubocop, brakeman, generators.", `{"type":"object","properties":{"command":{"type":"string"},"timeout_s":{"type":"integer"}},"required":["command"]}`),
		def("run_tests", "Prepare the database and run the full Rails test suite locally. Fast feedback — use before every verify.", `{"type":"object","properties":{}}`),
		def("commit_and_push", "Stage everything, commit with the message, and push to main.", `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		def("verify", "Run the authoritative Holodex verification of the pushed HEAD. Costs one ticket use; only call when local tests, rubocop and brakeman are green.", `{"type":"object","properties":{}}`),
		def("report", "Post a short human-readable progress line for the requester.", `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		def("done", "Finish the job. Call after a passing verification (or when giving up with an honest summary).", `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	}
}

func toolResultMsg(id, name, content string) chatMsg {
	if len(content) > 24_000 {
		content = content[:12_000] + "\n…[middle truncated]…\n" + content[len(content)-12_000:]
	}
	return chatMsg{Role: "tool", ToolCallID: id, Name: name, Content: content}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
