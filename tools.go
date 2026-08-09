package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
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
	authorID  string   // Discord user id of the requester — gates the coder allowlist
	Artifacts []string // file paths saved this turn
	usedWeb   bool     // this turn touched the web (fetch/search)
	usedCode  bool     // this turn ran code (shell/file) — mutually exclusive with web
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
		mk("tcg",
			"Look up any TCG card, set, or market price from the open rarebox-data dataset — Pokémon (EN/JP), Magic, Yu-Gi-Oh!, Lorcana, One Piece (EN/JP), Riftbound. To price/find a CARD, just pass its name as `query` (no `set`): it searches cards by name across the newest sets and returns each match's set id, number, rarity, USD price, and image URL — you do NOT need to know the set. Pass a set id as `set` to list that set's cards. This is the source of truth for a single card's price, JP included — use it, not web_search. Reserve web_search for SEALED products or live market chatter.",
			`{"type":"object","properties":{"game":{"type":"string","description":"pokemon|pokemon-ja|mtg|yugioh|lorcana|one-piece|one-piece-ja|riftbound"},"set":{"type":"string","description":"set id (e.g. me2); omit and pass query to search cards by name across sets"},"query":{"type":"string","description":"card name (searches across sets) or set name"}},"required":["game"]}`),
		mk("attach_image",
			"Fetch an image by URL and attach it to your Discord reply (e.g. a card image from a tcg lookup, or any picture the user asks to see). Images only, up to 8MB.",
			`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		mk("price_chart",
			"Build a historical price chart IMAGE (a PNG that displays inline in Discord) and attach it to your reply. kind=card|stock|crypto. "+
				"For a CARD: tcg-lookup it first, then pass game, set, number, and query=<card name> (uses rarebox price history, daily ~90d). "+
				"For CRYPTO: query=<name/symbol e.g. bitcoin> (CoinGecko). For a STOCK: symbol=<ticker e.g. AAPL> (Yahoo). "+
				"Optional days (default 30, cards 90). This is neutral price data — not advice.",
			`{"type":"object","properties":{"kind":{"type":"string","description":"card|stock|crypto"},"game":{"type":"string"},"set":{"type":"string"},"number":{"type":"string"},"symbol":{"type":"string","description":"stock ticker"},"query":{"type":"string","description":"crypto name/symbol, or card display name"},"days":{"type":"integer"}},"required":["kind"]}`),
	}
	if cfg.GithubEnabled() { // API only — no shell; gated by NANOCLAW_REPO_USERS (empty = everyone)
		defs = append(defs, mk("github",
			"Create and populate GitHub repos and open pull requests via the API, as Vela's own account. This is API-ONLY — nothing runs on the box. Actions: "+
				"create_repo {name, description?, private?} — makes a repo (with a README); "+
				"put_file {repo, path, content, message?, branch?} — writes/commits a file (repo is 'name' for Vela's own or 'owner/name'); "+
				"open_pr {repo:'owner/name', title, head, base?, body?} — head is 'branch' (same repo) or 'forkowner:branch' (from a fork); "+
				"fork {repo:'owner/name'}. To PR into someone else's repo: fork it, put_file onto a new branch in the fork, then open_pr on the upstream with head 'velaoc:branch'.",
			`{"type":"object","properties":{"action":{"type":"string","description":"create_repo|put_file|open_pr|fork"},"name":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"},"repo":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"},"message":{"type":"string"},"branch":{"type":"string"},"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"body":{"type":"string"}},"required":["action"]}`))
	}
	if cfg.CodeEnabled() { // gated to the coder allowlist (NANOCLAW_CODERS)
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
	return defs
}

type toolArgs struct {
	Query, URL, Name, Content, Note, Command, Path, Game, Set          string
	Action, Description, Repo, Message, Branch, Title, Head, Base, Body string
	Kind, Number, Symbol                                               string
	Days                                                               int
	Private                                                            bool
}

func (tc *ToolCtx) Run(name, args string) string {
	var a toolArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "tool error: bad arguments: " + err.Error()
	}
	// Injection guard: untrusted web content and box-writing actions (shell,
	// files, or GitHub writes) must never coexist in one turn, or a poisoned
	// page fetched this turn could drive the model into running commands or
	// pushing code. The two sides are mutually exclusive per turn (they persist
	// across the whole dive too).
	web := name == "web_search" || name == "fetch_url" || name == "tcg" || name == "price_chart"
	code := name == "shell" || name == "write_file" || name == "read_file" || name == "github"
	if web && tc.usedCode {
		return "REFUSED: web fetch blocked — this turn already ran code or a GitHub write, and untrusted page content must not mix with those. Do the browsing in a separate message."
	}
	if code && tc.usedWeb {
		return "REFUSED: blocked — this turn already fetched from the web, and a fetched page must not be able to reach a shell or push code (prompt-injection guard). Run this in a separate message from any browsing."
	}

	switch name {
	case "web_search":
		tc.usedWeb = true
		if tc.cfg.BraveKey != "" {
			return braveSearch(tc.cfg.BraveKey, a.Query)
		}
		return webSearch(a.Query)
	case "fetch_url":
		tc.usedWeb = true
		return fetchURL(a.URL)
	case "tcg":
		tc.usedWeb = true
		return tcgLookup(a.Game, a.Set, a.Query)
	case "attach_image":
		return tc.attachImage(a.URL)
	case "price_chart":
		tc.usedWeb = true
		return tc.priceChart(a)
	case "save_artifact":
		return tc.saveArtifact(a.Name, a.Content)
	case "remember":
		return appendMemory(tc.cfg, a.Note)
	case "github":
		tc.usedCode = true
		return tc.runGithub(a)
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

// braveSearch uses the Brave Search API (real JSON, not HTML scraping) and
// TRANSPARENTLY FALLS BACK to DuckDuckGo whenever Brave can't deliver — quota
// exhausted (429), a bad/expired key (401/403), an outage (5xx), a network
// error, or an empty/unparseable response. So hitting the Brave cap never
// stops web search; it just quietly degrades to DDG and Vela keeps working.
func braveSearch(key, query string) string {
	if out, ok := braveTry(key, query); ok {
		return out
	}
	log.Printf("brave search unavailable (quota/key/outage) — falling back to DuckDuckGo")
	return webSearch(query)
}

func braveTry(key, query string) (string, bool) {
	req, _ := http.NewRequest("GET", "https://api.search.brave.com/res/v1/web/search?count=8&q="+url.QueryEscape(query), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := ssrfClient.Do(req)
	if err != nil {
		return "", false // network error → DDG
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false // 429 quota / 401-403 key / 5xx outage → DDG
	}
	var d struct {
		Web struct {
			Results []struct{ Title, URL, Description string }
		} `json:"web"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err := json.Unmarshal(body, &d); err != nil || len(d.Web.Results) == 0 {
		return "", false // unparseable or empty → give DDG a shot
	}
	var b strings.Builder
	for i, r := range d.Web.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, stripTags(r.Description))
	}
	return b.String(), true
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

const maxImageBytes = 8 << 20 // Discord's baseline attachment limit

var imageExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
	"image/gif": ".gif", "image/svg+xml": ".svg",
}

// attachImage fetches an image URL (SSRF-guarded) and attaches it to the reply.
// Only image content-types, capped at Discord's limit. Binary bytes go to
// Discord, never into the model's context — so this isn't an injection vector.
func (tc *ToolCtx) attachImage(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "image error: absolute http(s) URL required"
	}
	resp, err := ssrfClient.Get(u)
	if err != nil {
		return "image error: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("image error: http %d", resp.StatusCode)
	}
	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	return tc.saveImage(filepath.Base(u), ct, resp.Body)
}

// saveImage validates the content-type, caps the size, and attaches — split
// out so it's testable without a live SSRF-guarded fetch.
func (tc *ToolCtx) saveImage(name, contentType string, body io.Reader) string {
	ext, ok := imageExt[contentType]
	if !ok {
		return "image error: not an image (content-type " + contentType + ")"
	}
	data, err := io.ReadAll(io.LimitReader(body, maxImageBytes+1))
	if err != nil {
		return "image error: " + err.Error()
	}
	if len(data) > maxImageBytes {
		return "image error: too large (>8MB)"
	}
	base := unsafeName.ReplaceAllString(name, "-")
	if !strings.Contains(base, ".") {
		base += ext
	}
	path := filepath.Join(tc.cfg.DataDir, "artifacts", fmt.Sprintf("%d-%s", time.Now().Unix(), base))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "image error: " + err.Error()
	}
	tc.Artifacts = append(tc.Artifacts, path)
	return fmt.Sprintf("fetched %s (%d KB) — it will be attached to your reply", base, len(data)/1024)
}
