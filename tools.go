package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The tool belt. Each call runs with a per-request context that collects
// artifacts so the Discord layer can attach whatever the agent produced.

type ToolCtx struct {
	cfg       *Config
	bankr     *Bankr
	keys      *KeyStore
	confirms  *Confirmations
	vault     *VaultClient
	authorID  string         // Discord user id of the requester — picks their wallet
	channelID string         // where to post a vault confirm button
	Artifacts []string       // file paths saved this turn
	Pending   *PendingAction // local-mode action awaiting the user's button click
	usedWeb   bool           // this turn touched the web (fetch/search)
	usedCode  bool           // this turn ran code (shell/file) — mutually exclusive with web
}

// clip truncates on a UTF-8 rune boundary so we never split a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + " …[truncated]"
}

func toolDefs(cfg *Config) []ToolDef {
	mk := func(name, desc, params string) ToolDef {
		var t ToolDef
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = json.RawMessage(params)
		return t
	}
	defs := []ToolDef{
		mk("web_search",
			"Search the web (DuckDuckGo). Use for benchmarks, model releases, docs, news. Returns titles, URLs, snippets.",
			`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		mk("fetch_url",
			"Fetch a web page and return its readable text (truncated). Use after web_search to read a source.",
			`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		mk("save_artifact",
			"Save a file the user gets as a Discord attachment. Use for HTML mockups (self-contained, inline CSS/JS), code files, markdown docs, diagrams. name must include an extension.",
			`{"type":"object","properties":{"name":{"type":"string"},"content":{"type":"string"}},"required":["name","content"]}`),
		mk("remember",
			"Append a durable note to long-term memory (survives reboots, shared across all channels). Use for server preferences, ongoing projects, decisions.",
			`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}`),
	}
	if cfg.CodeEnabled() { // off while this process holds wallet keys (interlock)
		defs = append(defs,
			mk("shell",
				"Run a shell command in your code workspace (persists across turns). Use for git (clone/commit/push to your GitHub), installing libraries, running builds/tests, scaffolding. 180s timeout. You run on a 256MB single-core RISC-V board — keep it light; for heavy builds, push and let CI compile. NOTE: code and web fetches can't run in the same turn (injection guard) — if you need to research first, do it in a separate message, then run code.",
				`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			mk("write_file",
				"Write a file in your workspace (creates dirs). Prefer this over shell heredocs for writing code.",
				`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			mk("read_file",
				"Read a file from your workspace.",
				`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`))
	}
	if cfg.WalletEnabled() {
		defs = append(defs, mk("bankr",
			"Bankr wallet + token agent (Base), acting on THE REQUESTER'S OWN connected wallet (via /connect) — never anyone else's. "+
				"Reads (balances, portfolio, prices, fees, address) run immediately. Anything that could move funds or deploy (launch, send, swap, buy, sell, trade, pay, move, convert, bridge, claim, stake…) does NOT execute here: it returns QUEUED and the user gets a Confirm button they must click. "+
				"So just describe the action plainly in your prompt; you do not approve it — the human's button click does.",
			`{"type":"object","properties":{"prompt":{"type":"string","description":"natural-language instruction for the requester's Bankr wallet"}},"required":["prompt"]}`))
	}
	return defs
}

func (tc *ToolCtx) Run(name, args string) string {
	var a struct {
		Query, URL, Name, Content, Note, Prompt, Command, Path string
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "tool error: bad arguments: " + err.Error()
	}
	// Injection guard: untrusted web content and code execution must never
	// coexist in one turn, or a poisoned page fetched this turn could inject
	// the shell commands the model then runs. The two sides are mutually
	// exclusive per turn (they persist across the whole dive too).
	web := name == "web_search" || name == "fetch_url"
	code := name == "shell" || name == "write_file" || name == "read_file"
	if web && tc.usedCode {
		return "REFUSED: web fetch blocked — this turn already ran code, and untrusted page content must not mix with code execution. Do the browsing in a separate message."
	}
	if code && tc.usedWeb {
		return "REFUSED: code blocked — this turn already fetched from the web, and a fetched page must not be able to reach a shell (prompt-injection guard). Run code in a separate message from any browsing."
	}

	switch name {
	case "web_search":
		tc.usedWeb = true
		return webSearch(a.Query)
	case "fetch_url":
		tc.usedWeb = true
		return fetchURL(a.URL)
	case "save_artifact":
		return tc.saveArtifact(a.Name, a.Content)
	case "remember":
		return appendMemory(tc.cfg, a.Note)
	case "bankr":
		return tc.runBankr(a.Prompt)
	case "shell":
		tc.usedCode = true
		return tc.runShell(a.Command)
	case "write_file":
		tc.usedCode = true
		return tc.writeWorkspaceFile(a.Path, a.Content)
	case "read_file":
		tc.usedCode = true
		return tc.readWorkspaceFile(a.Path)
	}
	return "tool error: unknown tool " + name
}

// runBankr acts ONLY on the requester's own connected wallet. Reads execute
// immediately; anything that isn't clearly a read is treated as fund-moving
// and routed to an out-of-band Discord confirmation — the model never
// approves a transaction, a button click by the requester does.
func (tc *ToolCtx) runBankr(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "bankr error: empty prompt"
	}
	// Vault mode: custody + policy + confirmation all live in clawvault. We can
	// only relay the prompt; a write returns queued and clawvault shows its own
	// button. We never see a key here.
	if tc.vault != nil {
		if !tc.vault.Connected(tc.authorID) {
			return "NOT CONNECTED: tell them to run /connect on the wallet bot (clawvault) and paste their own Bankr key — " +
				"it's entered there, never here, so I never touch it. Then I can read balances or relay a trade for them."
		}
		result, queued, err := tc.vault.Prompt(tc.authorID, tc.channelID, prompt)
		if err != nil {
			return "wallet error: " + err.Error()
		}
		if queued {
			return "QUEUED: the wallet bot has shown them a Confirm button for this exact action. Tell them to click Confirm there — " +
				"I can't approve it and neither can you; only their click runs it."
		}
		return result
	}
	if tc.bankr == nil || tc.keys == nil || !tc.keys.Usable() {
		return "bankr is not configured on this instance"
	}
	if !tc.keys.Has(tc.authorID) {
		return "NOT CONNECTED: this user has no wallet connected. Tell them to run /connect and paste their own Bankr API key " +
			"(from bankr.bot/api, wallet access enabled) — it's private and encrypted, and you'll only ever act on their own wallet."
	}
	if !isWalletRead(prompt) {
		// fail-closed: not clearly a read → treat as fund-moving, require a click
		tc.Pending = tc.confirms.Add(tc.authorID, prompt)
		return "QUEUED FOR CONFIRMATION: a Confirm button has been shown to the user for this exact action. " +
			"Do NOT say it's done — tell them to click Confirm (or Cancel). It runs only on their click."
	}
	key, _ := tc.keys.Get(tc.authorID)
	out, err := tc.bankr.Prompt(key, prompt)
	if err != nil {
		return "bankr error: " + err.Error()
	}
	return out
}

// blockedIP rejects anything that could reach the local box or its LAN —
// loopback, RFC1918/4193 private, link-local, unspecified, and CGNAT.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 { // 100.64.0.0/10 CGNAT
		return true
	}
	return false
}

// ssrfClient resolves and validates every dial (so redirects to an internal
// address are caught too) and dials the validated IP directly — no TOCTOU
// gap, and TLS SNI still uses the request host, not the IP.
var ssrfClient = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var target net.IP
			for _, ip := range ips {
				if blockedIP(ip) {
					return nil, fmt.Errorf("blocked address %s (private/loopback not allowed)", ip)
				}
				if target == nil {
					target = ip
				}
			}
			if target == nil {
				return nil, fmt.Errorf("no address for %s", host)
			}
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(target.String(), port))
		},
	},
}

func getPage(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nanoclaw/1.0)")
	resp, err := ssrfClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(b), err
}

var (
	ddgResult  = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)<a[^>]*class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe      = regexp.MustCompile(`(?s)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	anyTag     = regexp.MustCompile(`<[^>]*>`)
	wsRe       = regexp.MustCompile(`[ \t]*\n[ \t\n]*`)
)

func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = anyTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func webSearch(query string) string {
	page, err := getPage("https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query))
	if err != nil {
		return "search error: " + err.Error()
	}
	// Find each result link with its position, then take the snippet that
	// falls between this link and the next — so a result with no snippet
	// can't shift every later snippet onto the wrong result.
	locs := ddgResult.FindAllStringSubmatchIndex(page, 8)
	if len(locs) == 0 {
		return "no results"
	}
	var b strings.Builder
	for i, loc := range locs {
		href := page[loc[2]:loc[3]]
		title := stripTags(page[loc[4]:loc[5]])
		u := href
		if p, err := url.Parse(href); err == nil {
			if real := p.Query().Get("uddg"); real != "" {
				u = real
			}
		}
		segEnd := len(page)
		if i+1 < len(locs) {
			segEnd = locs[i+1][0]
		}
		snippet := ""
		if sm := ddgSnippet.FindStringSubmatch(page[loc[1]:segEnd]); sm != nil {
			snippet = stripTags(sm[1])
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, title, u, snippet)
	}
	return b.String()
}

func fetchURL(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "fetch error: absolute http(s) URL required"
	}
	page, err := getPage(u)
	if err != nil {
		return "fetch error: " + err.Error()
	}
	return clip(stripTags(wsRe.ReplaceAllString(page, "\n")), 6000)
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

const maxArtifactBytes = 1 << 20 // 1 MB — plenty for HTML/code/docs; bounds SD + runaway generations

func (tc *ToolCtx) saveArtifact(name, content string) string {
	name = unsafeName.ReplaceAllString(filepath.Base(name), "-")
	if name == "" || name == "." {
		return "artifact error: bad name"
	}
	if len(content) > maxArtifactBytes {
		return fmt.Sprintf("artifact error: too large (%d bytes, max %d) — trim it or split it", len(content), maxArtifactBytes)
	}
	path := filepath.Join(tc.cfg.DataDir, "artifacts",
		fmt.Sprintf("%d-%s", time.Now().Unix(), name))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "artifact error: " + err.Error()
	}
	tc.Artifacts = append(tc.Artifacts, path)
	return fmt.Sprintf("saved %s (%d bytes) — it will be attached to your reply", name, len(content))
}
