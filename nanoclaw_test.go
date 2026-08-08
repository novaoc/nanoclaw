package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testCfg(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"/artifacts", "/history"} {
		if err := os.MkdirAll(dir+d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Config{
		DeepseekURL: "http://unset", DeepseekKey: "test", Model: "deepseek-chat",
		DataDir: dir, FocusChannels: map[string]bool{}, MaxToolIters: 8, HistoryTurns: 4,
	}
}

// The full loop: model asks for save_artifact + remember, then answers.
// Verifies tool dispatch, artifact collection, memory, and history.
func TestAgentLoopWithTools(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		step++
		if step == 1 {
			if req.Messages[0].Role != "system" || !strings.Contains(req.Messages[0].Content, "nanoclaw") {
				t.Errorf("missing system prompt")
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"save_artifact","arguments":"{\"name\":\"mock.html\",\"content\":\"<h1>hi</h1>\"}"}},
				{"id":"c2","type":"function","function":{"name":"remember","arguments":"{\"note\":\"user builds rarebox\"}"}}]}}]}`))
			return
		}
		// second round must carry both tool results
		var toolMsgs int
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolMsgs++
			}
		}
		if toolMsgs != 2 {
			t.Errorf("expected 2 tool results, got %d", toolMsgs)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done — mockup attached"}}]}`))
	}))
	defer srv.Close()

	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	a := NewAgent(cfg)
	r := a.Handle("chan1", "wren", "make me a mockup")

	if r.Text != "done — mockup attached" {
		t.Fatalf("bad reply: %q", r.Text)
	}
	if len(r.Artifacts) != 1 || !strings.HasSuffix(r.Artifacts[0], "mock.html") {
		t.Fatalf("artifact not collected: %v", r.Artifacts)
	}
	if b, _ := os.ReadFile(r.Artifacts[0]); string(b) != "<h1>hi</h1>" {
		t.Fatalf("artifact content wrong")
	}
	if mem := readMemory(cfg); !strings.Contains(mem, "user builds rarebox") {
		t.Fatalf("memory not written: %q", mem)
	}
	// history persisted: user + assistant, no tool traffic
	h := NewHistory(cfg).Get("chan1")
	if len(h) != 2 || h[0].Role != "user" || h[1].Role != "assistant" {
		t.Fatalf("history wrong: %+v", h)
	}
}

func TestAgentToolBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// model loops forever on tool calls — the agent must cut it off
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"x","type":"function","function":{"name":"remember","arguments":"{\"note\":\"n\"}"}}]}}]}`))
	}))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	cfg.MaxToolIters = 3
	r := NewAgent(cfg).Handle("c", "u", "hi")
	if !strings.Contains(r.Text, "tool budget") {
		t.Fatalf("expected budget cutoff, got %q", r.Text)
	}
}

// /dive: the self-review pass must re-prompt with the critique and adopt
// the repaired answer.
func TestDiveSelfReviewPass(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls++
		if calls == 1 {
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "DIVE") {
				t.Errorf("dive marker missing from user message")
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"draft answer"}}]}`))
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if !strings.Contains(last.Content, "Review your answer") {
			t.Errorf("critique prompt missing on pass 2, got %.60q", last.Content)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"repaired answer"}}]}`))
	}))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	cfg.DiveToolIters, cfg.DivePasses = 16, 2
	r := NewAgent(cfg).Dive("c", "wren", "benchmark rundown")
	if r.Text != "repaired answer" {
		t.Fatalf("expected repaired answer, got %q", r.Text)
	}
	if calls != 2 {
		t.Fatalf("expected 2 passes, got %d", calls)
	}
	// history keeps the FINAL answer only
	h := NewHistory(cfg).Get("c")
	if len(h) != 2 || h[1].Content != "repaired answer" {
		t.Fatalf("history should hold the final answer: %+v", h)
	}
}

func TestSplitMessage(t *testing.T) {
	long := strings.Repeat("line one\n", 500) // 4500 chars
	chunks := splitMessage(long, 1990)
	if len(chunks) < 3 {
		t.Fatalf("expected ≥3 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 1990 {
			t.Fatalf("chunk over cap: %d", len(c))
		}
	}
	if got := splitMessage("short", 1990); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short passthrough broken")
	}
	// no-newline text must still split
	blob := strings.Repeat("x", 5000)
	for _, c := range splitMessage(blob, 1990) {
		if len(c) > 1990 {
			t.Fatalf("blob chunk over cap")
		}
	}
}

func TestDDGParsing(t *testing.T) {
	page := `<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fbench">DeepSeek V3 benchmarks</a>
	<a class="result__snippet">MMLU 88.5, GPQA 59.1 …</a>`
	links := ddgResult.FindAllStringSubmatch(page, 8)
	if len(links) != 1 || !strings.Contains(links[0][2], "DeepSeek") {
		t.Fatalf("result parse broken: %v", links)
	}
}

func TestArtifactNameSafety(t *testing.T) {
	cfg := testCfg(t)
	tc := &ToolCtx{cfg: cfg}
	out := tc.Run("save_artifact", `{"name":"../../etc/passwd","content":"x"}`)
	if strings.Contains(out, "error") {
		t.Fatalf("sanitized name should save: %s", out)
	}
	if strings.Contains(tc.Artifacts[0], "..") {
		t.Fatalf("path traversal not neutralized: %s", tc.Artifacts[0])
	}
	if !strings.HasPrefix(tc.Artifacts[0], cfg.DataDir) {
		t.Fatalf("artifact escaped data dir: %s", tc.Artifacts[0])
	}
}
