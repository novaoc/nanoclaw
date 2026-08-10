package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad ip %q", s)
	}
	return ip
}

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
			if req.Messages[0].Role != "system" || !strings.Contains(req.Messages[0].Content, "Vela") {
				t.Errorf("missing Vela system prompt")
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
	r := a.Handle("chan1", "u1", "wren", "make me a mockup")

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
	// The model loops forever on tool calls; the agent must cut it off and then
	// force a FINAL tools-off answer (no dead-end "ask me to continue").
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) == 0 {
			// the forced wrap-up call (tools disabled) → produce a partial answer
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"here's the partial answer from what I gathered"}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"x","type":"function","function":{"name":"remember","arguments":"{\"note\":\"n\"}"}}]}}]}`))
	}))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	cfg.MaxToolIters = 3
	r := NewAgent(cfg).Handle("c", "u1", "u", "hi")
	if !strings.Contains(r.Text, "partial answer from what I gathered") {
		t.Fatalf("expected a forced partial answer on budget exhaustion, got %q", r.Text)
	}
	if strings.Contains(strings.ToLower(r.Text), "ask me to continue") {
		t.Fatalf("must not dead-end with 'ask me to continue': %q", r.Text)
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
	cfg.Passes = 2
	r := NewAgent(cfg).Handle("c", "u1", "wren", "benchmark rundown")
	if r.Text != "repaired answer" {
		t.Fatalf("expected repaired answer, got %q", r.Text)
	}
	if calls != 2 {
		t.Fatalf("expected 2 passes (draft + 1 critique), got %d", calls)
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

func TestCodeCapabilityGating(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"dev": true}
	cfg.Workspace = t.TempDir()

	// tools only offered when a coder allowlist exists
	names := map[string]bool{}
	for _, d := range toolDefs(cfg) {
		names[d.Function.Name] = true
	}
	for _, n := range []string{"shell", "write_file", "read_file"} {
		if !names[n] {
			t.Fatalf("%s should be offered when coders are set", n)
		}
	}
	for _, d := range toolDefs(&Config{}) {
		if d.Function.Name == "shell" {
			t.Fatal("shell must NOT appear without a coder allowlist")
		}
	}

	coder := &ToolCtx{cfg: cfg, authorID: "dev"}
	rando := &ToolCtx{cfg: cfg, authorID: "rando"}

	// non-coder is refused before anything runs
	if out := rando.runShell("echo hi"); !strings.Contains(out, "REFUSED") {
		t.Fatalf("non-coder shell should be refused: %s", out)
	}
	if out := rando.writeWorkspaceFile("x.txt", "hi"); !strings.Contains(out, "REFUSED") {
		t.Fatalf("non-coder write should be refused: %s", out)
	}

	// coder can write + read + shell, confined to the workspace
	if out := coder.writeWorkspaceFile("sub/hello.txt", "hi vela"); !strings.Contains(out, "wrote") {
		t.Fatalf("coder write failed: %s", out)
	}
	if out := coder.readWorkspaceFile("sub/hello.txt"); out != "hi vela" {
		t.Fatalf("read mismatch: %q", out)
	}
	if out := coder.runShell("cat sub/hello.txt"); !strings.Contains(out, "hi vela") {
		t.Fatalf("shell cwd should be the workspace: %s", out)
	}

	// path confinement: cannot escape the workspace
	if out := coder.writeWorkspaceFile("../../etc/evil", "x"); !strings.Contains(out, "escapes") {
		t.Fatalf("path traversal on write not blocked: %s", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.Workspace, "..", "..", "etc", "evil")); err == nil {
		t.Fatal("traversal wrote outside the workspace")
	}
	if out := coder.readWorkspaceFile("../../../etc/passwd"); !strings.Contains(out, "escapes") {
		t.Fatalf("path traversal on read not blocked: %s", out)
	}
}

// Web fetches and code execution must be mutually exclusive within a phase,
// so a poisoned page can't inject the shell commands the model then runs.
func TestWebCodeInjectionGuard(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"dev": true}
	cfg.Workspace = t.TempDir()

	// fetch-then-shell: the shell must be deferred to an execution phase.
	tc := &ToolCtx{cfg: cfg, authorID: "dev"}
	_ = tc.Run("web_search", `{"query":"anything"}`) // may error at network; sets usedWeb
	if !tc.usedWeb {
		t.Fatal("web_search should mark the turn as web-touched")
	}
	if out := tc.Run("shell", `{"command":"echo hi"}`); !strings.Contains(out, "PHASE_BOUNDARY") {
		t.Fatalf("shell after a web fetch must cross a phase boundary: %s", out)
	}
	if tc.boundary == nil || tc.boundary.Lane != "code" || tc.usedCode {
		t.Fatalf("shell must remain unexecuted and request code phase: %+v", tc)
	}
	// Deploys are execution too: fetched content cannot flow directly into a
	// generated app bundle sent to Holodeck.
	tcDeploy := &ToolCtx{cfg: cfg, authorID: "dev", usedWeb: true}
	if out := tcDeploy.Run("deploy_demo", `{"name":"unsafe"}`); !strings.Contains(out, "PHASE_BOUNDARY") {
		t.Fatalf("deploy after web must cross a phase boundary: %s", out)
	}
	if tcDeploy.boundary == nil || tcDeploy.boundary.Lane != "code" || tcDeploy.usedCode {
		t.Fatalf("deploy must remain unexecuted and request code phase: %+v", tcDeploy)
	}

	// shell-then-fetch: the fetch must be deferred to a read-only phase.
	tc2 := &ToolCtx{cfg: cfg, authorID: "dev"}
	if out := tc2.Run("shell", `{"command":"echo hi"}`); strings.Contains(out, "REFUSED") {
		t.Fatalf("first shell should run, got: %s", out)
	}
	if out := tc2.Run("fetch_url", `{"url":"https://example.com"}`); !strings.Contains(out, "PHASE_BOUNDARY") {
		t.Fatalf("fetch after shell must cross a phase boundary: %s", out)
	}
	if tc2.boundary == nil || tc2.boundary.Lane != "web" || tc2.usedWeb {
		t.Fatalf("fetch must remain unexecuted and request web phase: %+v", tc2)
	}
}

// Crossing code -> web -> code is one user request. NanoClaw must make fresh
// internal phases and finish the build without waiting for another message.
func TestAutomaticWebCodePhaseContinuation(t *testing.T) {
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		step++
		write := func(body string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
		countRole := func(role string) int {
			n := 0
			for _, m := range req.Messages {
				if m.Role == role {
					n++
				}
			}
			return n
		}

		switch step {
		case 1:
			write(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"write-1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"phase-one.txt\",\"content\":\"started\"}"}}]}}]}`)
		case 2:
			write(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"defer-web","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://127.0.0.1/untrusted\"}"}}]}}]}`)
		case 3:
			if len(req.Tools) != 0 {
				t.Errorf("checkpoint must have tools disabled")
			}
			write(`{"choices":[{"message":{"role":"assistant","content":"The approved job has started; phase-one.txt exists. One factual web read remains, then finish phase-two.txt."}}]}`)
		case 4:
			if countRole("tool") != 0 {
				t.Errorf("raw code tool traffic crossed into web phase")
			}
			if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "READ-ONLY PHASE") {
				t.Errorf("missing automatic web continuation: %q", last)
			}
			write(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"web-read","type":"function","function":{"name":"fetch_url","arguments":"{\"url\":\"http://127.0.0.1/untrusted\"}"}}]}}]}`)
		case 5:
			write(`{"choices":[{"message":{"role":"assistant","content":"Read phase complete with no trusted remote content."}}]}`)
		case 6:
			if len(req.Tools) != 0 {
				t.Errorf("second checkpoint must have tools disabled")
			}
			write(`{"choices":[{"message":{"role":"assistant","content":"The read phase produced no trusted remote content. phase-one.txt exists and phase-two.txt remains."}}]}`)
		case 7:
			if countRole("tool") != 0 {
				t.Errorf("raw web tool traffic crossed into execution phase")
			}
			if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "EXECUTION PHASE") {
				t.Errorf("missing automatic code continuation: %q", last)
			}
			write(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"write-2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"phase-two.txt\",\"content\":\"finished\"}"}}]}}]}`)
		case 8:
			write(`{"choices":[{"message":{"role":"assistant","content":"done without another user reply"}}]}`)
		default:
			t.Errorf("unexpected LLM request %d", step)
			write(`{"choices":[{"message":{"role":"assistant","content":"unexpected"}}]}`)
		}
	}))
	defer srv.Close()

	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	cfg.Workspace = t.TempDir()
	cfg.Coders = map[string]bool{"dev": true}
	reply := NewAgent(cfg).Handle("phase-channel", "dev", "wren", "build the app and look up one fact")

	if reply.Text != "done without another user reply" {
		t.Fatalf("automatic continuation did not finish: %q", reply.Text)
	}
	if step != 8 {
		t.Fatalf("expected eight model calls across three phases, got %d", step)
	}
	for path, want := range map[string]string{"phase-one.txt": "started", "phase-two.txt": "finished"} {
		got, err := os.ReadFile(filepath.Join(cfg.Workspace, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestRbNormNum(t *testing.T) {
	cases := map[string]string{"001/217": "1", "TG07": "tg7", "SWSH001": "swsh1", "0": "0", "13": "13", "  25/198 ": "25"}
	for in, want := range cases {
		if got := rbNormNum(in); got != want {
			t.Errorf("rbNormNum(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAttachImage(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	// a real image content-type attaches with the right extension
	if out := tc.saveImage("card", "image/png", strings.NewReader("\x89PNG fake")); !strings.Contains(out, "attached") {
		t.Fatalf("png should attach: %s", out)
	}
	if len(tc.Artifacts) != 1 || !strings.HasSuffix(tc.Artifacts[0], ".png") {
		t.Fatalf("artifact not .png: %v", tc.Artifacts)
	}
	// a non-image content-type is refused and doesn't attach
	if out := tc.saveImage("page", "text/html", strings.NewReader("<html>")); !strings.Contains(out, "not an image") {
		t.Fatalf("non-image refused: %s", out)
	}
	if len(tc.Artifacts) != 1 {
		t.Fatal("non-image must not attach")
	}
	// SSRF: an internal target is refused at fetch time, before any save
	if out := tc.attachImage("http://169.254.169.254/img.png"); !strings.Contains(out, "error") {
		t.Fatalf("SSRF image fetch should error: %s", out)
	}
	// non-URL rejected
	if out := tc.attachImage("not-a-url"); !strings.Contains(out, "http") {
		t.Fatalf("bad url should be rejected: %s", out)
	}
}

func TestSSRFBlocking(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.1.1", "0.0.0.0", "100.100.0.1", "::1", "fe80::1"}
	for _, s := range blocked {
		if !blockedIP(mustIP(t, s)) {
			t.Errorf("should be blocked: %s", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"}
	for _, s := range allowed {
		if blockedIP(mustIP(t, s)) {
			t.Errorf("should be allowed: %s", s)
		}
	}
	// fetch_url must reject a literal private host before dialing
	if out := fetchURL("http://169.254.169.254/latest/meta-data/"); !strings.Contains(out, "error") {
		t.Fatalf("SSRF to link-local should error, got: %s", out)
	}
}

func TestClipRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 100) // 2 bytes each
	out := clip(s, 5)             // 5 bytes lands mid-rune
	if !utf8.ValidString(strings.TrimSuffix(out, " …[truncated]")) {
		t.Fatalf("clip split a rune: %q", out)
	}
	if clip("short", 100) != "short" {
		t.Fatal("clip should pass short strings through")
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
