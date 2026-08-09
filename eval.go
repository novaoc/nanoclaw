package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// Headless eval harness: drive the real agent loop over a list of prompts,
// one at a time, and log per-request latency, resident-set size, artifacts,
// and the reply. Runs on the board so the actual 128 MB RAM takes the load.
// Invoked as `nanoclaw eval <prompts.jsonl> <out.jsonl>`. Each prompt line is
// {"n":1,"cat":"card","ch":"c1","msg":"...","coder":false}. Same ch across
// lines shares history (multi-turn context tests); a fresh ch is isolated.

type evalPrompt struct {
	N     int    `json:"n"`
	Cat   string `json:"cat"`
	Ch    string `json:"ch"`
	Msg   string `json:"msg"`
	Coder bool   `json:"coder"` // run this one with coder tools enabled
	Img   string `json:"img"`   // optional image URL for vision
}

type evalResult struct {
	N          int      `json:"n"`
	Cat        string   `json:"cat"`
	Msg        string   `json:"msg"`
	Reply      string   `json:"reply"`
	Artifacts  []string `json:"artifacts"`
	ArtBytes   []int    `json:"art_bytes"`
	LatencyMs  int64    `json:"latency_ms"`
	RSSKB      int64    `json:"rss_kb"`      // process resident set after the turn
	HeapKB     int64    `json:"heap_kb"`     // Go heap in use after the turn
	Empty      bool     `json:"empty"`       // reply was blank / (no reply)
	ModelErr   bool     `json:"model_err"`   // reply carries a ⚠️ model error
	Attached   bool     `json:"attached"`    // reply text claims an attachment
	MissingArt bool     `json:"missing_art"` // claimed attachment but produced none
}

// rssKB reads VmRSS from /proc/self/status (Linux/board); 0 elsewhere.
func rssKB() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:")), "%d", &kb)
			return kb
		}
	}
	return 0
}

func claimsAttachment(s string) bool {
	l := strings.ToLower(s)
	for _, kw := range []string{"attached", "chart's attached", "chart is attached", "here's the chart", "posted it", "image attached"} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

func runEval(promptsPath, outPath string) {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval config:", err)
		os.Exit(1)
	}
	// isolate eval state from the live bot
	cfg.DataDir = cfg.DataDir + "-eval"
	_ = os.MkdirAll(cfg.DataDir+"/artifacts", 0o755)
	_ = os.MkdirAll(cfg.DataDir+"/history", 0o755)
	cfg.Workspace = cfg.DataDir + "/workspace"

	pf, err := os.Open(promptsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval prompts:", err)
		os.Exit(1)
	}
	defer pf.Close()
	var prompts []evalPrompt
	sc := bufio.NewScanner(pf)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p evalPrompt
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			fmt.Fprintln(os.Stderr, "skip bad prompt line:", err)
			continue
		}
		prompts = append(prompts, p)
	}

	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval out:", err)
		os.Exit(1)
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	agent := NewAgent(cfg)
	testUID := "eval-user"
	for _, p := range prompts {
		// coder tools are per-request: flip the allowlist for coder prompts only
		if p.Coder {
			cfg.Coders = map[string]bool{testUID: true}
		} else {
			cfg.Coders = map[string]bool{}
		}
		var imgs []string
		if p.Img != "" {
			imgs = []string{p.Img}
		}
		start := time.Now()
		var reply Reply
		if strings.HasPrefix(p.Msg, "/dive ") {
			reply = agent.Dive(p.Ch, testUID, "aregus", strings.TrimPrefix(p.Msg, "/dive "))
		} else {
			reply = agent.Handle(p.Ch, testUID, "aregus", p.Msg, imgs...)
		}
		lat := time.Since(start).Milliseconds()

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		r := evalResult{
			N: p.N, Cat: p.Cat, Msg: p.Msg, Reply: reply.Text,
			LatencyMs: lat, RSSKB: rssKB(), HeapKB: int64(ms.HeapInuse / 1024),
		}
		for _, a := range reply.Artifacts {
			r.Artifacts = append(r.Artifacts, a)
			if fi, err := os.Stat(a); err == nil {
				r.ArtBytes = append(r.ArtBytes, int(fi.Size()))
			} else {
				r.ArtBytes = append(r.ArtBytes, -1)
			}
		}
		t := strings.TrimSpace(reply.Text)
		r.Empty = t == "" || t == "(no reply)"
		r.ModelErr = strings.Contains(reply.Text, "⚠️") || strings.HasPrefix(t, "Hit my tool limit")
		r.Attached = claimsAttachment(reply.Text)
		r.MissingArt = r.Attached && len(reply.Artifacts) == 0
		_ = enc.Encode(r)
		_ = out.Sync()
		// progress to stderr so a tail shows liveness
		fmt.Fprintf(os.Stderr, "[%3d/%d] %-10s %5dms rss=%dMB art=%d%s%s\n",
			p.N, len(prompts), p.Cat, lat, r.RSSKB/1024, len(reply.Artifacts),
			iff(r.ModelErr, " MODELERR", ""), iff(r.MissingArt, " MISSING-ART", ""))
		runtime.GC() // keep peak honest between requests, like the real one-at-a-time bot
	}
	fmt.Fprintln(os.Stderr, "eval done ->", outPath)
}

func iff(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
