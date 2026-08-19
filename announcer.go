package main

// The announcer: one loop, all builds, no babysitters.
//
// The per-job poller era kept dying — 55-minute cliffs, board restarts
// killing in-process goroutines, out-of-band recoveries the thread never
// heard about ("Vela never replied with the demo"). This replaces all of it:
// the worker owns each build's lifecycle including the deploy, the board
// persists only {job → thread} on disk, and a single long-poll parked on the
// worker's change feed turns every transition into a post in the right
// thread. Restart-proof by construction — the truth lives on the worker, the
// mapping lives on the SD card, and boot catch-up replays anything missed.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type workerJobNote struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	// LastState dedups announcements across restarts and feed replays.
	LastState string `json:"last_state,omitempty"`
	// Attempts counts failed verifications already spent on this build, so
	// the auto-fix loop is bounded: one repair turn, then the honest ❌.
	Attempts int `json:"attempts,omitempty"`
}

var workerJobsMu sync.Mutex

func workerJobsPath(cfg *Config) string { return cfg.DataDir + "/worker-jobs.json" }

func loadWorkerJobs(cfg *Config) map[string]workerJobNote {
	workerJobsMu.Lock()
	defer workerJobsMu.Unlock()
	out := map[string]workerJobNote{}
	if b, err := os.ReadFile(workerJobsPath(cfg)); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveWorkerJobs(cfg *Config, m map[string]workerJobNote) {
	workerJobsMu.Lock()
	defer workerJobsMu.Unlock()
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return
	}
	tmp := workerJobsPath(cfg) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, workerJobsPath(cfg))
	}
}

// rememberWorkerJob records the job→thread mapping at enqueue time. This is
// the only state the board keeps about a build.
func rememberWorkerJob(cfg *Config, job string, note workerJobNote) {
	m := loadWorkerJobs(cfg)
	m[job] = note
	saveWorkerJobs(cfg, m)
}

type workerFeedJob struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
	SHA       string `json:"sha"`
	URL       string `json:"url"`
	QueuePos  int    `json:"queue_pos"`
	ChannelID string `json:"channel_id"`
}

// watchWorkerJobs is started once at bot boot. It parks a long-poll on the
// worker's change feed and posts transitions into mapped threads.
func (b *Bot) watchWorkerJobs() {
	if !b.cfg.WorkerEnabled() {
		return
	}
	var cursor int64
	// Boot catch-up: force a full pass so anything that finished while the
	// board was down or restarting still gets announced. since=0 returns all
	// live jobs.
	for {
		if atomicLoadDraining(b) {
			return
		}
		jobs, next, err := b.fetchWorkerChanges(cursor)
		if err != nil {
			time.Sleep(20 * time.Second)
			continue
		}
		cursor = next
		if len(jobs) == 0 {
			continue // long-poll timed out quietly; park again
		}
		b.announceWorkerJobs(jobs)
	}
}

func atomicLoadDraining(b *Bot) bool { return atomic.LoadInt32(&b.draining) != 0 }

// repairTurnContent is the synthetic turn fed to Vela after a failed
// verification — shared by the live announcer and pipetest so the self-test
// exercises the exact repair path production runs.
func repairTurnContent(name, repo, detail string) string {
	return fmt.Sprintf(
		"SYSTEM REPAIR TURN (not a user message): your build %q (repo %s) failed Holodex verification. "+
			"The distilled failure output is below. Read ONLY the files named in the failures, apply the "+
			"smallest fix with put_file/patch_file, then call enqueue_build again with the same repo and name. "+
			"THE TURN IS NOT DONE UNTIL enqueue_build HAS BEEN CALLED — a diagnosis without a re-enqueue is a "+
			"failed turn, and there is no one else to act on your analysis. Do not tour the repo, do not "+
			"re-read passing code, do not start over, do not create a new repo. Fix → push → enqueue_build.\n\n%s",
		name, repo, detail)
}

// runFixTurn gives Vela one bounded repair turn after a failed verification.
// She reads the distilled failure, fixes the repo through her normal tools,
// and re-enqueues; the fresh job inherits attempts=1 so a second failure ends
// in the honest ❌ instead of a loop.
func (b *Bot) runFixTurn(note workerJobNote, j workerFeedJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("fix turn panic: %v", r)
		}
	}()
	coder := ""
	for id := range b.cfg.Coders {
		coder = id
		break
	}
	if coder == "" {
		return
	}
	release, _ := b.takeLane(coder, "fix:"+note.Name, 0, 0)
	defer release()

	content := repairTurnContent(note.Name, note.Repo, j.Detail)
	reply := b.agent.HandleTurn(Turn{
		ChannelID: note.Channel,
		AuthorID:  coder,
		Author:    "build-repair",
		Notify:    func(s string) { _, _ = b.PostMessage(note.Channel, s) },
		SetBuildName: func(s string) {
			if b.build != nil {
				b.build.setName(s)
			}
		},
	}, content)
	if strings.TrimSpace(reply.Text) != "" {
		_, _ = b.PostMessage(note.Channel, reply.Text)
	}
	// The repair turn's enqueue_build wrote a fresh mapping entry for this
	// repo; stamp the spent attempt on it so the loop stays bounded.
	m := loadWorkerJobs(b.cfg)
	changed := false
	for id, n := range m {
		if strings.EqualFold(n.Repo, note.Repo) && n.Attempts <= note.Attempts {
			n.Attempts = note.Attempts + 1
			m[id] = n
			changed = true
		}
	}
	if changed {
		saveWorkerJobs(b.cfg, m)
	}
}

func (b *Bot) fetchWorkerChanges(cursor int64) ([]workerFeedJob, int64, error) {
	url := fmt.Sprintf("%s/jobs-changes?since=%d&wait=55", b.cfg.WorkerURL, cursor)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, cursor, err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.WorkerToken)
	client := *ssrfClient
	client.Timeout = 70 * time.Second // outlives the 55s park
	resp, err := client.Do(req)
	if err != nil {
		return nil, cursor, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("changes %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Cursor int64           `json:"cursor"`
		Jobs   []workerFeedJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, cursor, err
	}
	return out.Jobs, out.Cursor, nil
}

func (b *Bot) announceWorkerJobs(jobs []workerFeedJob) {
	m := loadWorkerJobs(b.cfg)
	changed := false
	for _, j := range jobs {
		note, known := m[j.ID]
		if !known {
			// Not one of ours (a probe, or enqueued by another instance).
			continue
		}
		if j.State == note.LastState {
			continue
		}
		msg := ""
		switch j.State {
		case "queued":
			if j.QueuePos > 0 {
				msg = fmt.Sprintf("⏳ %s is queued behind %d build(s) — it starts automatically.", note.Name, j.QueuePos)
			}
		case "verifying":
			msg = "🔬 " + note.Name + ": " + j.Detail
		case "deployed":
			msg = fmt.Sprintf("🚀 **%s** is live: %s\nRepo (yours to keep): https://github.com/%s — the demo deck wipes daily at 3AM Mexico City.", note.Name, j.URL, note.Repo)
		case "failed":
			detail := j.Detail
			if i := strings.IndexByte(detail, '\n'); i >= 0 {
				detail = detail[:i]
			}
			if note.Attempts < 1 {
				// Vela codes without a local test run (a 256MB board can't
				// hold a Rails suite), so the first verification failure is
				// part of the loop, not the end of it: hand her the distilled
				// failure once and let her repair and re-enqueue. One bounded
				// attempt — after that the ❌ is honest.
				msg = "🔧 " + note.Name + " failed verification — reading the failure and fixing it now."
				if _, err := b.PostMessage(note.Channel, msg); err != nil {
					log.Printf("announcer post %s: %v", j.ID, err)
					continue
				}
				go b.runFixTurn(note, j)
				delete(m, j.ID)
				changed = true
				continue
			}
			msg = "❌ " + note.Name + " didn't make it: " + detail + "\nWork-in-progress is preserved on GitHub — I can pick it up from here if you want."
		case "verified":
			// Verified with no deploy ticket (legacy) — say so honestly.
			msg = "✅ " + note.Name + " passed verification (no auto-deploy grant on this job — ask me to deploy it)."
		}
		if msg != "" {
			if _, err := b.PostMessage(note.Channel, msg); err != nil {
				log.Printf("announcer post %s: %v", j.ID, err)
				continue // keep LastState unchanged so we retry on next change
			}
		}
		note.LastState = j.State
		if j.State == "deployed" || j.State == "failed" {
			delete(m, j.ID)
		} else {
			m[j.ID] = note
		}
		changed = true
	}
	if changed {
		saveWorkerJobs(b.cfg, m)
	}
}
