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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// model loops forever on tool calls — the agent must cut it off
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"x","type":"function","function":{"name":"remember","arguments":"{\"note\":\"n\"}"}}]}}]}`))
	}))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.DeepseekURL = srv.URL
	cfg.MaxToolIters = 3
	r := NewAgent(cfg).Handle("c", "u1", "u", "hi")
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
	r := NewAgent(cfg).Dive("c", "u1", "wren", "benchmark rundown")
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

// Read classification is fail-CLOSED: only clear look-ups run without a
// confirm. Verb-denylist misses ("yeet", "move", "pay", "convert") must NOT
// be treated as reads.
func TestWalletReadClassification(t *testing.T) {
	reads := []string{"what are my balances?", "show my portfolio", "price of ETH",
		"how much are my fees?", "list my deployed tokens", "what's my wallet address?"}
	writes := []string{"send 0.1 ETH to vitalik.eth", "launch a token called VELA",
		"swap 100 USDC for ETH", "buy $50 of DEGEN", "claim my fees",
		// the denylist-evaders from the review:
		"yeet all my ETH to 0xabc", "move everything to my cold wallet",
		"pay alice 20 USDC", "convert my DEGEN to ETH", "liquidate my position",
		// no recognizable read intent at all → fail closed to write
		"do the thing with my coins"}
	for _, r := range reads {
		if !isWalletRead(r) {
			t.Errorf("should be a READ: %q", r)
		}
	}
	for _, w := range writes {
		if isWalletRead(w) {
			t.Errorf("should NOT be a read (fail closed): %q", w)
		}
	}
}

func TestBankrPerUserGating(t *testing.T) {
	cfg := testCfg(t)
	cfg.Secret = "test-server-secret" // enables key custody + the tool

	// the tool is only offered when custody is enabled
	names := map[string]bool{}
	for _, d := range toolDefs(cfg) {
		names[d.Function.Name] = true
	}
	if !names["bankr"] {
		t.Fatal("bankr tool should be offered when NANOCLAW_SECRET is set")
	}
	for _, d := range toolDefs(&Config{}) {
		if d.Function.Name == "bankr" {
			t.Fatal("bankr tool must NOT appear without custody enabled")
		}
	}

	ks := NewKeyStore(cfg)
	confirms := NewConfirmations()
	newTC := func(uid string) *ToolCtx {
		return &ToolCtx{cfg: cfg, bankr: NewBankr(cfg), keys: ks, confirms: confirms, authorID: uid}
	}

	// a user with NO connected wallet → NOT CONNECTED, never hits the network
	if out := newTC("nobody").runBankr("what are my balances?"); !strings.Contains(out, "NOT CONNECTED") {
		t.Fatalf("unconnected user should get NOT CONNECTED, got: %s", out)
	}
	// connect a user, then a WRITE → QUEUED for a button, and a Pending set
	if err := ks.Put("alice", "bk_alicealicealice"); err != nil {
		t.Fatal(err)
	}
	tc := newTC("alice")
	if out := tc.runBankr("send 1 ETH to 0xabc"); !strings.Contains(out, "QUEUED") {
		t.Fatalf("connected write should queue a confirmation, got: %s", out)
	}
	if tc.Pending == nil || tc.Pending.UID != "alice" {
		t.Fatalf("write should stage a pending action for the requester")
	}
	// the model can't approve it; only the owner's button click can.
	if _, err := confirms.Take(tc.Pending.Token, "mallory"); err != errConfirmNotYours {
		t.Fatalf("only the requester may confirm, got: %v", err)
	}
	if _, err := confirms.Take(tc.Pending.Token, "alice"); err != nil {
		t.Fatalf("requester should be able to take their own action: %v", err)
	}
	// alice's READ passes the gate (fails only at the fake network) — no queue
	tc2 := newTC("alice")
	if out := tc2.runBankr("what are my balances?"); strings.Contains(out, "NOT CONNECTED") || strings.Contains(out, "QUEUED") {
		t.Fatalf("connected read should execute, got: %s", out)
	}
	if tc2.Pending != nil {
		t.Fatal("a read must not stage a pending action")
	}
	// bob (unconnected) still can't act even though alice is connected
	if out := newTC("bob").runBankr("send 1 ETH to 0xabc"); !strings.Contains(out, "NOT CONNECTED") {
		t.Fatalf("bob must not borrow alice's wallet, got: %s", out)
	}
}

// The out-of-band confirmation is what actually authorizes a transaction.
func TestConfirmationOwnerOnly(t *testing.T) {
	c := NewConfirmations()
	pa := c.Add("alice", "send 1 ETH to 0xabc")
	// wrong user can't consume or cancel, and the action survives for the owner
	if _, err := c.Take(pa.Token, "eve"); err != errConfirmNotYours {
		t.Fatalf("wrong clicker should be rejected, got %v", err)
	}
	if err := c.Cancel(pa.Token, "eve"); err != errConfirmNotYours {
		t.Fatalf("wrong clicker cancel should be rejected, got %v", err)
	}
	got, err := c.Take(pa.Token, "alice")
	if err != nil || got.Prompt != "send 1 ETH to 0xabc" {
		t.Fatalf("owner take failed: %v %+v", err, got)
	}
	// one-shot: a second take fails
	if _, err := c.Take(pa.Token, "alice"); err != errConfirmExpired {
		t.Fatalf("token should be single-use, got %v", err)
	}
}

// Keys must be unreadable on disk without the server secret, and bound to
// their owner's Discord id (AAD) so a swapped file can't impersonate.
func TestKeyStoreEncryption(t *testing.T) {
	cfg := testCfg(t)
	cfg.Secret = "server-secret-A"
	ks := NewKeyStore(cfg)
	if err := ks.Put("alice", "bk_supersecretkey123"); err != nil {
		t.Fatal(err)
	}
	// on-disk file is ciphertext — the plaintext key must not appear
	raw, _ := os.ReadFile(ks.path("alice"))
	if strings.Contains(string(raw), "bk_supersecretkey123") {
		t.Fatal("plaintext key leaked to disk")
	}
	// round-trips with the right secret
	if k, ok := ks.Get("alice"); !ok || k != "bk_supersecretkey123" {
		t.Fatalf("decrypt failed: %q ok=%v", k, ok)
	}
	// a DIFFERENT secret cannot decrypt the same file (fresh store, no cache)
	cfg2 := *cfg
	cfg2.DataDir = cfg.DataDir // same files
	cfg2.Secret = "server-secret-B"
	if k, ok := NewKeyStore(&cfg2).Get("alice"); ok {
		t.Fatalf("wrong secret decrypted the key: %q", k)
	}
	// no secret at all → custody disabled, nothing stored or served
	cfg3 := *cfg
	cfg3.Secret = ""
	if NewKeyStore(&cfg3).Usable() {
		t.Fatal("keystore should be unusable without a secret")
	}
	// delete wipes it
	if !ks.Delete("alice") || ks.Has("alice") {
		t.Fatal("delete should remove the key")
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

	// interlock: with wallet keys held in-process, code is off entirely
	locked := *cfg
	locked.Secret = "in-process-secret"
	if locked.CodeEnabled() {
		t.Fatal("code must be disabled while NANOCLAW_SECRET is held in-process")
	}
	for _, d := range toolDefs(&locked) {
		if d.Function.Name == "shell" {
			t.Fatal("shell must not be offered when the custody interlock is tripped")
		}
	}
	if out := (&ToolCtx{cfg: &locked, authorID: "dev"}).runShell("echo hi"); !strings.Contains(out, "SEPARATE processes") {
		t.Fatalf("interlock should refuse shell even for a coder: %s", out)
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

// Web fetches and code execution must be mutually exclusive within a turn,
// so a poisoned page can't inject the shell commands the model then runs.
func TestWebCodeInjectionGuard(t *testing.T) {
	cfg := testCfg(t)
	cfg.Coders = map[string]bool{"dev": true}
	cfg.Workspace = t.TempDir()

	// fetch-then-shell: the shell must be refused
	tc := &ToolCtx{cfg: cfg, authorID: "dev"}
	_ = tc.Run("web_search", `{"query":"anything"}`) // may error at network; sets usedWeb
	if !tc.usedWeb {
		t.Fatal("web_search should mark the turn as web-touched")
	}
	if out := tc.Run("shell", `{"command":"echo hi"}`); !strings.Contains(out, "REFUSED") {
		t.Fatalf("shell after a web fetch must be refused: %s", out)
	}

	// shell-then-fetch: the fetch must be refused
	tc2 := &ToolCtx{cfg: cfg, authorID: "dev"}
	if out := tc2.Run("shell", `{"command":"echo hi"}`); strings.Contains(out, "REFUSED") {
		t.Fatalf("first shell should run, got: %s", out)
	}
	if out := tc2.Run("fetch_url", `{"url":"https://example.com"}`); !strings.Contains(out, "REFUSED") {
		t.Fatalf("fetch after shell must be refused: %s", out)
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
