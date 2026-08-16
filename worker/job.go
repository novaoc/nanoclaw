package main

// The job runner: one goroutine per accepted job, serialized by the store's
// single build slot. Clone → agent loop → (the agent pushes and verifies) →
// final state for the board's poller.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	maxToolIterations = 60
	jobWallClock      = 45 * time.Minute
	// Consecutive read-only tool calls tolerated before the loop insists on
	// implementation. Enough to read the spec, routes, a controller, and a
	// neighbouring test; not enough to tour the whole foundation.
	maxConsecutiveReads = 14
)

func (s *server) runJob(id string) {
	s.store.slot <- struct{}{}
	defer func() { <-s.store.slot }()

	s.store.mu.Lock()
	job, ok := s.store.jobs[id]
	if !ok {
		s.store.mu.Unlock()
		return
	}
	repo, name := job.Repo, job.Name
	spec, instructions := job.spec, job.instructions
	s.store.mu.Unlock()

	ws := &workspace{root: filepath.Join(s.cfg.Data, id), repo: repo}
	defer os.RemoveAll(ws.root)
	if err := os.MkdirAll(ws.root, 0o755); err != nil {
		s.fail(id, "workspace: "+err.Error())
		return
	}

	s.store.update(id, func(j *buildJob) { j.State = "coding"; j.Detail = "cloning " + repo })
	if err := s.cloneRepo(ws); err != nil {
		s.fail(id, err.Error())
		return
	}
	writeWorkspaceEnvNote(ws)

	messages := []chatMsg{
		{Role: "system", Content: workerSystemPrompt},
		{Role: "user", Content: fmt.Sprintf(
			"Repository: %s (already cloned into your workspace)\nApp name: %s\n\nSpecification:\n%s\n\nRequest context from the person who asked:\n%s",
			repo, name, spec, instructions)},
	}
	tools := agentTools()
	deadline := time.Now().Add(jobWallClock)
	reads := 0

	for iter := 0; iter < maxToolIterations; iter++ {
		if time.Now().After(deadline) {
			s.fail(id, "job wall clock exceeded")
			return
		}
		msg, err := s.chat(messages, tools)
		if err != nil {
			s.fail(id, "model error: "+err.Error())
			return
		}
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			// A bare text turn — nudge back into the loop rather than dying.
			log.Printf("job %s iter %d: no tool calls (%.80q)", id, iter, msg.Content)
			messages = append(messages, chatMsg{Role: "user",
				Content: "Continue with tool calls. When finished, call done."})
			continue
		}

		finished := false
		for _, call := range msg.ToolCalls {
			// Reading is cheap and therefore seductive: a model will happily
			// tour a 300-file Rails app forever. Count consecutive read-only
			// calls and cut the tour off once it has plenty of context —
			// Vela's board learned the same lesson and calls it
			// INSPECTION_COMPLETE.
			switch call.Function.Name {
			case "list_tree", "read_file", "shell":
				reads++
			default:
				reads = 0
			}
			started := time.Now()
			result := s.execTool(id, ws, call)
			if reads == maxConsecutiveReads {
				result += "\n\nINSPECTION_COMPLETE: you have read enough of this repository. " +
					"Stop exploring and start implementing with write_file now; run_tests will tell you what you still need to know."
			}
			// Without this the loop was completely opaque: a job could spend
			// ten minutes exploring and look identical to one wedged on a
			// clone. Every call is logged, and read-only calls still move the
			// status line so pollers see life.
			log.Printf("job %s iter %d: %s (%s) -> %.100q",
				id, iter, call.Function.Name, time.Since(started).Round(time.Millisecond), firstLine(result))
			s.store.update(id, func(j *buildJob) {
				if j.State == "coding" {
					j.Detail = "working: " + call.Function.Name
				}
			})
			messages = append(messages, toolResultMsg(call.ID, call.Function.Name, result))
			if call.Function.Name == "done" {
				finished = true
			}
		}
		if finished {
			// State was set by the verify/done handlers; anything not
			// verified by now is a failure with the agent's own summary.
			s.store.mu.Lock()
			if j := s.store.jobs[id]; j != nil && j.State != "verified" {
				j.State = "failed"
				j.Updated = time.Now()
			}
			s.store.mu.Unlock()
			return
		}
	}
	s.fail(id, "tool budget exhausted without done")
}

func (s *server) fail(id, detail string) {
	log.Printf("job %s failed: %s", id, firstLine(detail))
	s.store.update(id, func(j *buildJob) { j.State = "failed"; j.Detail = detail })
}

func (s *server) execTool(id string, ws *workspace, call toolCall) string {
	var args struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Command  string `json:"command"`
		TimeoutS int    `json:"timeout_s"`
		Message  string `json:"message"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "tool error: bad arguments: " + err.Error()
	}

	switch call.Function.Name {
	case "list_tree":
		out, err := ws.listTree(400)
		if err != nil {
			return "error: " + err.Error()
		}
		return out

	case "read_file":
		out, err := ws.readFile(args.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		return out

	case "write_file":
		if err := ws.writeFile(args.Path, args.Content); err != nil {
			return "error: " + err.Error()
		}
		return "wrote " + args.Path

	case "shell":
		timeout := 10 * time.Minute
		if args.TimeoutS > 0 && args.TimeoutS <= 600 {
			timeout = time.Duration(args.TimeoutS) * time.Second
		}
		out, err := ws.runShell(args.Command, timeout)
		if err != nil {
			return fmt.Sprintf("exit error: %v\n%s", err, out)
		}
		return out

	case "run_tests":
		s.store.update(id, func(j *buildJob) { j.Detail = "running the local test suite" })
		out, err := ws.runTests()
		if err != nil {
			return fmt.Sprintf("TESTS FAILED (%v)\n%s", err, out)
		}
		return "TESTS PASSED\n" + out

	case "commit_and_push":
		sha, err := s.commitAll(ws, args.Message)
		if err != nil {
			return "error: " + err.Error()
		}
		if err := s.push(ws); err != nil {
			return "error: " + err.Error()
		}
		s.store.update(id, func(j *buildJob) { j.SHA = sha; j.Detail = "pushed " + sha[:12] })
		return "pushed " + sha

	case "verify":
		sha, err := s.headSHA(ws)
		if err != nil {
			return "error: cannot resolve HEAD: " + err.Error()
		}
		s.store.update(id, func(j *buildJob) {
			j.State = "verifying"
			j.SHA = sha
			j.VerifiesUsed++
			j.Detail = "Holodex verification of " + sha[:12]
		})
		res, err := s.ticketVerify(s.snapshot(id), sha)
		if err != nil {
			s.store.update(id, func(j *buildJob) { j.State = "coding"; j.Detail = "verify error" })
			return "verify error: " + err.Error()
		}
		if !res.OK {
			s.store.update(id, func(j *buildJob) { j.State = "coding"; j.Detail = "verification failed" })
			return fmt.Sprintf("Verification FAILED: %s\n%s", res.Error, tailOf(res.Logs, 6000))
		}
		s.store.update(id, func(j *buildJob) {
			j.State = "verified"
			j.Receipt = res.Receipt
			j.Detail = fmt.Sprintf("verification passed (%d files, %.0fs)", res.Files, float64(res.DurationMS)/1000)
		})
		return fmt.Sprintf("Verification PASSED for %s (receipt captured, %d files). Call done.", sha[:12], res.Files)

	case "report":
		s.store.update(id, func(j *buildJob) { j.Detail = args.Message })
		return "noted"

	case "done":
		s.store.update(id, func(j *buildJob) {
			if j.State == "verified" {
				j.Detail = args.Summary
			} else {
				j.Detail = "gave up: " + args.Summary
			}
		})
		return "finished"

	default:
		return "tool error: unknown tool " + call.Function.Name
	}
}

func (s *server) snapshot(id string) *buildJob {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	j := *s.store.jobs[id]
	return &j
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
