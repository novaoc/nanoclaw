package main

// The job runner: one goroutine per accepted job, serialized by the store's
// single build slot. Clone → agent loop → (the agent pushes and verifies) →
// final state for the board's poller.

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	// The iteration ceiling is a runaway backstop, not a work budget. A job
	// that is progressing gets to keep going — the sandclock build was
	// guillotined at 60 iterations WHILE inspecting its diff to commit, which
	// taught us that counting effort is the wrong guard entirely.
	maxToolIterations = 400
	// The one real ceiling: bounds worst-case model spend and how long one
	// job can hold the single build slot.
	jobWallClock = 2 * time.Hour
	// What actually kills a job early: stagnation. This many consecutive
	// iterations without a single progress event (a write, a suite-state
	// change, a commit, a verify) means the loop is circling, not building.
	maxStagnantIters = 25
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
	repo, sha := job.Repo, job.SHA
	s.store.mu.Unlock()

	// Deterministic pipeline — no model anywhere. The worker used to carry a
	// full coding agent, and every single production failure came from that
	// brain (empty completions, design loops, budget deaths) while the dumb
	// half worked every time. 2026-08-19: the brain was removed on request —
	// "just deploy shit and that's all." Vela writes the code; this verifies
	// and ships it.
	if sha == "" {
		s.fail(id, "no sha provided — the board resolves HEAD at enqueue")
		return
	}

	s.store.update(id, func(j *buildJob) {
		j.State = "verifying"
		j.Detail = "Holodex verification of " + sha[:12]
	})
	snap := s.snapshot(id)
	res, err := s.ticketVerify(snap, sha)
	if err != nil {
		s.fail(id, "verification error: "+firstLine(err.Error()))
		return
	}
	s.store.update(id, func(j *buildJob) { j.VerifiesUsed++ })
	if !res.OK {
		// Distilled, not raw: the board feeds this detail straight into
		// Vela's repair turn, and minitest failure blocks are signal while
		// docker build noise is not.
		s.fail(id, "verification FAILED: "+res.Error+"\n"+distillMinitest(res.Logs, 1500))
		return
	}
	s.store.update(id, func(j *buildJob) {
		j.Receipt = res.Receipt
		j.Detail = fmt.Sprintf("verification passed (%d files, %.0fs)", res.Files, float64(res.DurationMS)/1000)
	})

	if snap.deployTicket == "" {
		s.store.update(id, func(j *buildJob) { j.State = "verified"; j.Detail += " — no deploy grant" })
		return
	}
	url, err := s.ticketDeploy(s.snapshot(id), sha, res.Receipt)
	if err != nil {
		s.store.update(id, func(j *buildJob) {
			j.State = "verified"
			j.Detail = "verified; deploy failed: " + firstLine(err.Error())
		})
		return
	}
	s.store.update(id, func(j *buildJob) {
		j.State = "deployed"
		j.URL = url
		j.Detail = "live at " + url
	})
	log.Printf("job %s: deployed %s@%s at %s", id, repo, sha[:12], url)
}

func (s *server) fail(id, detail string) {
	log.Printf("job %s failed: %s", id, firstLine(detail))
	s.store.update(id, func(j *buildJob) { j.State = "failed"; j.Detail = detail })
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

// distillMinitest reduces raw `rails test` output to what a model can act on:
// every Error:/Failure: block (deduplicated) plus the run summary, capped at
// budget bytes. The full stream — dots, seed lines, deprecations — taught us
// on 2026-08-16 that a 40 KB tool result is worse than no result.
func distillMinitest(out string, budget int) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	var blocks []string
	seen := map[string]bool{}
	summary := ""
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.Contains(t, " runs, ") && strings.Contains(t, " assertions") {
			summary = t
		}
		if t != "Error:" && t != "Failure:" {
			continue
		}
		var block []string
		for j := i; j < len(lines) && j < i+12; j++ {
			if j > i && strings.TrimSpace(lines[j]) == "" {
				break
			}
			block = append(block, lines[j])
		}
		i += len(block) - 1
		b := strings.Join(block, "\n")
		if !seen[b] {
			seen[b] = true
			blocks = append(blocks, b)
		}
	}
	res := summary
	if len(blocks) > 0 {
		res = summary + "\n\n" + strings.Join(blocks, "\n\n")
	}
	if res == "" {
		res = out // nothing recognisable — better the tail than silence
		if len(res) > budget {
			res = "…" + res[len(res)-budget:]
		}
		return res
	}
	if len(res) > budget {
		res = res[:budget] + "\n…[more failures truncated — fix these first]"
	}
	return res
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
