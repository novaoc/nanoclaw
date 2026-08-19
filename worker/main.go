// vela-worker — the build half of Vela's request pipeline.
//
// Vela (on the 256 MB board) frames requests, forks the foundation, signs a
// job ticket, and enqueues here. This daemon — on the fat Hetzner box, in its
// own container — clones the repo, runs the coding agent with a real shell
// and the foundation test suite locally, pushes commits, and burns its
// ticket budget on Holodex verifications until one passes. It never holds
// the Holodex build secret and cannot deploy: it reports {sha, receipt} and
// Vela's board signs the actual deploy.
//
// Environment:
//
//	WORKER_TOKEN           bearer for this API (required)
//	WORKER_ADDR            listen address (default :8790)
//	WORKER_DATA            job workspaces root (default /work)
//	DEEPSEEK_API_KEY       model key (required; spend-capped, worker-only)
//	DEEPSEEK_API_URL       OpenAI-compatible base (default https://api.deepseek.com)
//	WORKER_MODEL           model id (default deepseek-chat)
//	GITHUB_TOKEN           scoped push token for generated repos (required)
//	HOLODEX_URL            e.g. https://api.holode.xyz (required)
//	HOLODEX_TOKEN          Holodex bearer (required; NOT the build secret)
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	Token        string
	Addr         string
	Data         string
	ModelKey     string
	ModelURL     string
	Model        string
	VisionModel  string
	GitHubToken  string
	HolodexURL   string
	HolodexToken string
}

func loadConfig() config {
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := config{
		Token:        get("WORKER_TOKEN", ""),
		Addr:         get("WORKER_ADDR", ":8790"),
		Data:         get("WORKER_DATA", "/work"),
		ModelKey:     get("DEEPSEEK_API_KEY", ""),
		ModelURL:     strings.TrimSuffix(get("DEEPSEEK_API_URL", "https://api.deepseek.com"), "/"),
		Model:        get("WORKER_MODEL", "deepseek-chat"),
		VisionModel:  get("WORKER_VISION_MODEL", ""),
		GitHubToken:  get("GITHUB_TOKEN", ""),
		HolodexURL:   strings.TrimSuffix(get("HOLODEX_URL", ""), "/"),
		HolodexToken: get("HOLODEX_TOKEN", ""),
	}
	var missing []string
	for k, v := range map[string]string{
		"WORKER_TOKEN": c.Token, "DEEPSEEK_API_KEY": c.ModelKey,
		"GITHUB_TOKEN": c.GitHubToken, "HOLODEX_URL": c.HolodexURL, "HOLODEX_TOKEN": c.HolodexToken,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c
}

// buildJob is one enqueued build. Everything the board's poller reads lives
// in the exported fields.
type buildJob struct {
	ID           string    `json:"id"`
	Repo         string    `json:"repo"` // owner/name
	Name         string    `json:"name"`
	State        string    `json:"state"`  // queued | coding | verifying | verified | deployed | failed
	Detail       string    `json:"detail"` // last human-readable status line
	SHA          string    `json:"sha"`
	Receipt      string    `json:"receipt"`
	Port         int       `json:"port"`
	URL          string    `json:"url"`        // live demo URL once deployed
	ChannelID    string    `json:"channel_id"` // opaque board-side thread id, echoed back
	QueuePos     int       `json:"queue_pos"`  // 0 = running/done; N = builds ahead
	Seq          int64     `json:"seq"`        // bumped on every update (change feed cursor)
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	VerifiesUsed int       `json:"verifies_used"`

	ticket       string
	deployTicket string
	spec         string
	instructions string
}

type store struct {
	mu   sync.Mutex
	jobs map[string]*buildJob
	// One build at a time: local test suites are heavy and the box also
	// serves production. Raise later if the box proves bored.
	slot chan struct{}
	// Change feed: seq bumps on every job update; waiters holds parked
	// long-polls that are released (closed) on any change.
	seq     int64
	waiters chan struct{}
}

func newStore() *store {
	return &store{jobs: map[string]*buildJob{}, slot: make(chan struct{}, 1), waiters: make(chan struct{})}
}

func (st *store) update(id string, fn func(*buildJob)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if j, ok := st.jobs[id]; ok {
		fn(j)
		j.Updated = time.Now()
		st.seq++
		j.Seq = st.seq
		// Release every parked long-poll: close-and-replace broadcast.
		close(st.waiters)
		st.waiters = make(chan struct{})
	}
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("worker: no entropy: " + err.Error())
	}
	return "wj-" + hex.EncodeToString(b)
}

type server struct {
	cfg   config
	store *store
	// previews holds each job's running dev server (see preview.go). Guarded
	// by store.mu; one entry at a time in practice (single build slot).
	previews map[string]*exec.Cmd
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo         string `json:"repo"`
		Name         string `json:"name"`
		Ticket       string `json:"ticket"`
		Spec         string `json:"spec"`
		Instructions string `json:"instructions"`
		Port         int    `json:"port"`
		JobID        string `json:"job_id"` // must match the ticket's job field
		DeployTicket string `json:"deploy_ticket"`
		ChannelID    string `json:"channel_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Repo == "" || req.Name == "" || req.Ticket == "" || req.JobID == "" {
		http.Error(w, "repo, name, ticket and job_id are required", http.StatusBadRequest)
		return
	}
	// The ticket is opaque to the worker (Holodex validates it), but its job
	// field is the id both sides share — insist they match so status polling
	// and budget accounting can never diverge.
	if parts := strings.SplitN(req.Ticket, ":", 2); len(parts) < 1 || parts[0] != req.JobID {
		http.Error(w, "ticket job id does not match job_id", http.StatusBadRequest)
		return
	}
	if req.Port == 0 {
		req.Port = 80
	}

	job := &buildJob{
		ID: req.JobID, Repo: req.Repo, Name: req.Name, Port: req.Port,
		State: "queued", Detail: "queued", Created: time.Now(), Updated: time.Now(),
		ChannelID: req.ChannelID,
		ticket:    req.Ticket, deployTicket: req.DeployTicket,
		spec: req.Spec, instructions: req.Instructions,
	}
	s.store.mu.Lock()
	if _, exists := s.store.jobs[job.ID]; exists {
		s.store.mu.Unlock()
		http.Error(w, "job id already exists", http.StatusConflict)
		return
	}
	// One live build per repository. An agent that keeps working after the
	// hand-off (seen 2026-08-16: Vela re-ran the whole flow and enqueued the
	// same repo twice) would otherwise queue a duplicate build that rebuilds
	// and redeploys everything. Answer with the job already in flight so the
	// caller's poller still tracks something real.
	for _, existing := range s.store.jobs {
		if existing.Repo == job.Repo && existing.State != "failed" && existing.State != "verified" {
			id := existing.ID
			s.store.mu.Unlock()
			log.Printf("duplicate enqueue for %s — returning live job %s", job.Repo, id)
			writeJSON(w, http.StatusAccepted, map[string]any{
				"id": id, "state": "queued", "duplicate": true,
			})
			return
		}
	}
	s.store.jobs[job.ID] = job
	s.store.mu.Unlock()

	go s.runJob(job.ID)
	log.Printf("enqueued %s for %s", job.ID, job.Repo)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "state": "queued"})
}

// queuePosLocked counts non-terminal jobs created before j — its place in
// line. Call with store.mu held.
func (st *store) queuePosLocked(j *buildJob) int {
	if j.State != "queued" {
		return 0
	}
	pos := 0
	for _, other := range st.jobs {
		if other.ID == j.ID {
			continue
		}
		switch other.State {
		case "failed", "deployed", "verified":
			continue
		}
		if other.Created.Before(j.Created) {
			pos++
		}
	}
	return pos
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.store.mu.Lock()
	j, ok := s.store.jobs[r.PathValue("id")]
	var out buildJob
	if ok {
		out = *j
		out.QueuePos = s.store.queuePosLocked(j)
	}
	s.store.mu.Unlock()
	if !ok {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		log.Fatal(err)
	}
	s := &server{cfg: cfg, store: newStore()}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.auth(s.handleEnqueue))
	mux.HandleFunc("GET /jobs/{id}", s.auth(s.handleStatus))
	mux.HandleFunc("GET /jobs-changes", s.auth(s.handleChanges))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("vela-worker up on %s (model=%s, holodex=%s)", cfg.Addr, cfg.Model, cfg.HolodexURL)
	log.Fatal(http.ListenAndServe(cfg.Addr, mux))
}

// handleChanges is the board's ping-back channel, NAT-inverted: the board
// parks one outbound long-poll here, and any job transition answers it
// immediately. `since` is the last seq the caller saw; `wait` seconds bounds
// the park. Empty answer = nothing changed, re-poll.
func (s *server) handleChanges(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > 55 {
		wait = 55
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)

	for {
		s.store.mu.Lock()
		var changed []buildJob
		for _, j := range s.store.jobs {
			if j.Seq > since {
				jj := *j
				jj.QueuePos = s.store.queuePosLocked(j)
				changed = append(changed, jj)
			}
		}
		cursor := s.store.seq
		waiter := s.store.waiters
		s.store.mu.Unlock()

		if len(changed) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"cursor": cursor, "jobs": changed})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"cursor": cursor, "jobs": []buildJob{}})
			return
		}
		select {
		case <-waiter: // something changed — loop and collect it
		case <-time.After(remaining):
		}
	}
}
