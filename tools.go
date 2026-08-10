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
	guildID   string   // guild of the current turn (for Discord actions)
	channelID string   // channel of the current turn
	disc      Discord  // Discord actuator (nil in headless/eval)
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
			"Build a historical price chart IMAGE (a PNG that displays inline in Discord) and attach it to your reply. kind=card|stock|crypto|index. "+
				"For a CARD: tcg-lookup it first, then pass game, set, number, and query=<card name> (uses rarebox price history, daily ~90d). "+
				"For CRYPTO: query=<name/symbol e.g. bitcoin> (CoinGecko). For a STOCK: symbol=<ticker e.g. AAPL> (Yahoo). "+
				"For an INDEX (a whole game's market, Card Ladder-style): kind=index + game — charts an equal-weight base-100 index of that game's tracked cards (newest sets), e.g. 'how's the pokemon market doing?'. "+
				"Optional days (default 30, cards/index 90). The output is a STATIC PNG — it has no hover or interactivity, so never describe it as interactive. "+
				"Card history covers EVERY set that still trades, VINTAGE INCLUDED — never refuse an old card as unchartable; call this and let it answer. "+
				"SANITY-CHECK every card chart: if the chart's latest price differs wildly from the price you quoted, you charted the WRONG PRINTING — redo with the correct number instead of shipping it with a caveat. "+
				"This is neutral price data — not advice.",
			`{"type":"object","properties":{"kind":{"type":"string","description":"card|stock|crypto|index"},"game":{"type":"string"},"set":{"type":"string"},"number":{"type":"string"},"symbol":{"type":"string","description":"stock ticker"},"query":{"type":"string","description":"crypto name/symbol, or card display name"},"days":{"type":"integer"}},"required":["kind"]}`),
		mk("bench_chart",
			"Render an LLM benchmark comparison as a grouped-bar chart IMAGE (a PNG that displays inline in Discord) attached to your reply. "+
				"Use AFTER researching real, dated scores with web_search/fetch_url — you pass the numbers in; NEVER guess a score. "+
				"scores align 1:1 with benchmarks; use null where a model doesn't report that benchmark. "+
				"Keep model order stable across re-renders (append newly requested models at the end) so each model keeps its color.",
			`{"type":"object","properties":{"title":{"type":"string","description":"chart title, e.g. 'DeepSeek-V4 vs Llama 3.1 405B'"},"source":{"type":"string","description":"short provenance note with a date, e.g. 'vendor model cards · Aug 2026'"},"benchmarks":{"type":"array","items":{"type":"string"},"description":"benchmark names, e.g. MMLU-Pro, GPQA Diamond, SWE-bench Verified (max 10)"},"models":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"scores":{"type":"array","items":{"type":["number","null"]}}},"required":["name","scores"]},"description":"one entry per model (max 8), scores aligned to benchmarks"}},"required":["benchmarks","models"]}`),
		mk("model_releases",
			"Check Hugging Face for recent model releases from the labs worth tracking (DeepSeek, Qwen, Meta Llama, Mistral, Google, OpenAI oss, Microsoft, etc.). "+
				"Optional query filters to one lab/name (e.g. 'qwen', 'deepseek'). Returns model id, lab, date, and downloads, newest first, and flags which are NEW since the last check. Use for 'any new models out?' / 'what did deepseek just drop?'.",
			`{"type":"object","properties":{"query":{"type":"string","description":"optional: a lab or name to filter (qwen|deepseek|llama|...)"}}}`),
		mk("discord_forum",
			"Post in a Discord FORUM channel on request. action=post creates a new forum post/thread (needs channel=<forum name or id>, title, body); action=reply adds a message to an existing post/thread (needs thread=<thread id or its title/name>, body). Use when asked to 'post an intro in the introductions forum' or 'reply to that forum post'. Write the content in Vela's voice.",
			`{"type":"object","properties":{"action":{"type":"string","description":"post|reply"},"channel":{"type":"string","description":"forum channel name or id (for post)"},"title":{"type":"string","description":"post title (for post)"},"thread":{"type":"string","description":"thread id or title (for reply)"},"body":{"type":"string","description":"the message content"}},"required":["action","body"]}`),
	}
	if cfg.XAIEnabled() { // Grok image/video generation (needs XAI_API_KEY)
		defs = append(defs,
			mk("generate_image",
				"Generate image(s) with Grok (xAI) and attach them to your reply. Use when asked to 'make/draw/generate a picture/image of ...'. prompt = what to create (be vivid and specific); n = how many (1-4, default 1); optional image_url to use a reference image to edit/riff on. Costs money per image — generate what's asked, don't spam extras.",
				`{"type":"object","properties":{"prompt":{"type":"string"},"n":{"type":"integer","description":"1-4"},"image_url":{"type":"string","description":"optional reference image to edit/riff on"}},"required":["prompt"]}`),
			mk("generate_video",
				"Generate a short video with Grok (xAI) and attach it. Use when asked to 'make/generate a video/clip of ...' or to animate an image. prompt = what happens in the clip; optional image_url to animate a still; duration = seconds as a string (default '5', max '15'). Async — it renders over several seconds. Costs money per second — keep clips short unless asked.",
				`{"type":"object","properties":{"prompt":{"type":"string"},"image_url":{"type":"string","description":"optional still image to animate"},"duration":{"type":"string","description":"seconds, e.g. '5' (max '15')"}},"required":["prompt"]}`))
	}
	if len(cfg.Mods) > 0 { // moderation only when a mod allowlist is configured
		defs = append(defs, mk("moderate",
			"Discord moderation — ONLY act on an explicit request from an authorized moderator. action=timeout|kick|ban|delete|slowmode. "+
				"user=<@mention/id/username> for timeout/kick/ban; duration like '10m'/'1h'/'1d' for timeout; reason is logged; "+
				"delete removes a specific message (message=<id> in the current channel); slowmode sets per-user seconds (seconds=0 clears) on the current channel. Always state what you did and why.",
			`{"type":"object","properties":{"action":{"type":"string","description":"timeout|kick|ban|delete|slowmode"},"user":{"type":"string"},"duration":{"type":"string","description":"timeout length e.g. 10m, 1h, 1d"},"reason":{"type":"string"},"message":{"type":"string","description":"message id to delete"},"seconds":{"type":"integer","description":"slowmode: per-user seconds (0 clears)"},"days":{"type":"integer","description":"ban: how many days of the user's messages to delete"}},"required":["action"]}`))
	}
	if cfg.GithubEnabled() { // API only — no shell; gated by NANOCLAW_REPO_USERS (empty = everyone)
		defs = append(defs, mk("github",
			"Create and populate GitHub repos, open pull requests, and PUBLISH LIVE WEBSITES via the API, as Vela's own account. This is API-ONLY — nothing runs on the box. Actions: "+
				"create_repo {name, description?, private?} — makes a repo (with a README); "+
				"put_file {repo, path, content, message?, branch?} — writes/commits a file (repo is 'name' for Vela's own or 'owner/name'); "+
				"open_pr {repo:'owner/name', title, head, base?, body?} — head is 'branch' (same repo) or 'forkowner:branch' (from a fork); "+
				"fork {repo:'owner/name'}; "+
				"enable_pages {repo} — turns on GitHub Pages and returns the live URL. "+
				"TO DEPLOY A SITE someone asks to put online: create_repo (public) → put_file index.html (self-contained HTML) → enable_pages → give them the live link (takes ~a minute to go live). "+
				"To PR into someone else's repo: fork it, put_file onto a new branch in the fork, then open_pr on the upstream with head 'velaoc:branch'.",
			`{"type":"object","properties":{"action":{"type":"string","description":"create_repo|put_file|open_pr|fork"},"name":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"},"repo":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"},"message":{"type":"string"},"branch":{"type":"string"},"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"body":{"type":"string"}},"required":["action"]}`))
	}
	if cfg.SandboxURL != "" && cfg.SandboxToken != "" { // holodeck demo hosting
		defs = append(defs, mk("deploy_demo",
			"Deploy a static web app/site to Vela's own demo host and get a LIVE URL on her domain, instantly. Use when someone wants a working demo online — the usual ask is 'make me X' → build it, push the code to a repo (github tool), AND deploy_demo so they get both the repo and a live link. "+
				"files = the complete app: an index.html at the root (required, self-contained or referencing the other files by relative path), plus any css/js/assets. Static only — no server code runs. "+
				"Demos SELF-DESTRUCT after 7 days (say so when sharing the link); the GitHub repo is the permanent copy.",
			`{"type":"object","properties":{"name":{"type":"string","description":"short app name — becomes the subdomain slug"},"files":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string","description":"relative path, e.g. index.html or css/style.css"},"content":{"type":"string"}},"required":["path","content"]}}},"required":["name","files"]}`))
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
		names := cfg.Secrets.Names()
		desc := "Wipe deploy secrets from memory and disk when a deploy/task is done (do this as soon as you no longer need them). "
		if len(names) > 0 {
			desc += "Currently held (values hidden, injected into shell as these env vars): " + strings.Join(names, ", ") + "."
		} else {
			desc += "None are set right now — they're added on the box, not in chat."
		}
		defs = append(defs, mk("clear_secrets", desc, `{"type":"object","properties":{}}`))
	}
	return defs
}

// readOnlyToolNames are the pure lookups a self-review pass may always use:
// they inform a better text answer with NO side effect.
var readOnlyToolNames = map[string]bool{
	"web_search": true, "fetch_url": true, "tcg": true, "model_releases": true,
}

// freeArtifactToolNames render/attach LOCALLY with no cost and no external side
// effect. They're offered in critique passes ONLY until one artifact exists this
// turn — so a chart the draft ran out of budget to make can still be produced,
// exactly once, without duplication. (Paid/side-effecting tools — Grok image/
// video gen, moderation, forum posts, github, shell — are NEVER in critique.)
var freeArtifactToolNames = map[string]bool{
	"bench_chart": true, "price_chart": true, "save_artifact": true, "attach_image": true,
}

// critiqueTools is the belt for a self-review pass. haveArtifact drops the local
// artifact builders once something's been produced, preventing duplicates.
func critiqueTools(cfg *Config, haveArtifact bool) []ToolDef {
	var out []ToolDef
	for _, d := range toolDefs(cfg) {
		n := d.Function.Name
		if readOnlyToolNames[n] || (!haveArtifact && freeArtifactToolNames[n]) {
			out = append(out, d)
		}
	}
	return out
}

type toolArgs struct {
	Query, URL, Name, Content, Note, Command, Path, Game, Set           string
	Action, Description, Repo, Message, Branch, Title, Head, Base, Body string
	Kind, Number, Symbol, Source                                        string
	User, Reason, Channel, Thread, Duration                             string // moderation + forum
	Prompt                                                              string // xAI image/video gen
	// snake_case keys need explicit tags — Go's JSON matching ignores case but
	// NOT underscores, so without this "image_url" silently unmarshals to "".
	ImageURL   string `json:"image_url"`
	Days       int
	Seconds    int // slowmode seconds
	N          int // image count
	Private    bool
	Benchmarks []string
	Models     []benchModel
	Files      []demoFile // deploy_demo: the app's files
}

type demoFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// unwrapArgs undoes DeepSeek's occasional double-wrapping of tool arguments,
// where the real args arrive nested under a lone "arguments" key —
// {"arguments":"{\"path\":...}"} or {"arguments":{...}} — instead of at the top
// level. Left unwrapped, every field reads empty and file/artifact tools fail
// silently (seen killing a /dive's save_artifact). Only unwraps the exact lone-
// "arguments" shape, so real args that happen to contain an "arguments" field
// are untouched.
func unwrapArgs(s string) string {
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &probe) != nil || len(probe) != 1 {
		return s
	}
	inner, ok := probe["arguments"]
	if !ok {
		return s
	}
	var asStr string
	if json.Unmarshal(inner, &asStr) == nil {
		return asStr // was a JSON string: {"arguments":"{...}"}
	}
	return string(inner) // was a nested object: {"arguments":{...}}
}

func (tc *ToolCtx) Run(name, args string) string {
	args = unwrapArgs(args)
	var a toolArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "tool error: bad arguments: " + err.Error()
	}
	// Injection guard: untrusted web content and box-writing actions (shell,
	// files, or GitHub writes) must never coexist in one turn, or a poisoned
	// page fetched this turn could drive the model into running commands or
	// pushing code. The two sides are mutually exclusive per turn (they persist
	// across the whole dive too).
	// attach_image counts as web: its bytes never reach the model, but it IS an
	// arbitrary outbound GET — after a code turn (which can read env/tokens) it
	// would otherwise be an exfiltration channel via the URL.
	web := name == "web_search" || name == "fetch_url" || name == "tcg" || name == "price_chart" || name == "attach_image" || name == "model_releases" || name == "generate_image" || name == "generate_video"
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
		tc.usedWeb = true
		return tc.attachImage(a.URL)
	case "price_chart":
		tc.usedWeb = true
		return tc.priceChart(a)
	case "bench_chart":
		// renders only from the args the model passes — no fetch, so it's
		// neither a web nor a code action for the injection guard
		return tc.benchChart(a)
	case "save_artifact":
		return tc.saveArtifact(a.Name, a.Content)
	case "remember":
		return appendMemory(tc.cfg, a.Note)
	case "clear_secrets":
		return tc.clearSecrets()
	case "model_releases":
		tc.usedWeb = true
		return tc.modelReleases(a.Query)
	case "moderate":
		return tc.moderate(a)
	case "discord_forum":
		return tc.discordForum(a)
	case "generate_image":
		tc.usedWeb = true
		return tc.generateImage(a)
	case "generate_video":
		tc.usedWeb = true
		return tc.generateVideo(a)
	// The code handlers set usedCode themselves AFTER their allowlist gates
	// pass — a REFUSED call ran nothing, so it must not poison the rest of the
	// turn (blocking every later web tool with "this turn already ran code").
	case "github":
		return tc.runGithub(a)
	case "deploy_demo":
		return tc.deployDemo(a)
	case "shell":
		return tc.runShell(a.Command)
	case "write_file":
		return tc.writeWorkspaceFile(a.Path, a.Content)
	case "read_file":
		return tc.readWorkspaceFile(a.Path)
	}
	return "tool error: unknown tool " + name
}

// blockedIP rejects anything that could reach the local box or its LAN —
// loopback, RFC1918/4193 private, link-local, unspecified, and CGNAT.
func blockedIP(ip net.IP) bool {
	// stdlib checks all call To4() internally, so IPv4-mapped IPv6
	// (::ffff:192.168.1.1) is unwrapped and caught here too.
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// v4 (and mapped v6, which To4 unwraps) ranges the stdlib doesn't cover.
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8 — Linux routes the whole block to localhost
			return true
		case v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255: // limited broadcast
			return true
		case v4[0] == 100 && v4[1]&0xc0 == 64: // 100.64.0.0/10 CGNAT
			return true
		}
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
		fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
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
	// name from the URL PATH only — a query string ("card.png?ex=…") would
	// otherwise end up inside the filename and break inline display in Discord
	name := u
	if p, err := url.Parse(u); err == nil && p.Path != "" {
		name = p.Path
	}
	return tc.saveImage(filepath.Base(name), ct, resp.Body)
}

// saveImage validates the content-type, caps the size, and attaches — split
// out so it's testable without a live SSRF-guarded fetch.
func (tc *ToolCtx) saveImage(name, contentType string, body io.Reader) string {
	ext, ok := imageExt[contentType]
	if !ok {
		// %q + cap: the header is attacker-controlled text headed for model context
		return fmt.Sprintf("image error: not an image (content-type %.60q)", contentType)
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
	path := filepath.Join(tc.cfg.DataDir, "artifacts", fmt.Sprintf("%d-%s", time.Now().UnixNano(), base))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "image error: " + err.Error()
	}
	tc.Artifacts = append(tc.Artifacts, path)
	return fmt.Sprintf("fetched %s (%d KB) — it will be attached to your reply", base, len(data)/1024)
}
