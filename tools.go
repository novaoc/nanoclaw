package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The tool belt. Each call runs with a per-request context that collects
// artifacts so the Discord layer can attach whatever the agent produced.

type ToolCtx struct {
	cfg       *Config
	bankr     *Bankr
	authorID  string   // Discord user id of the requester (for admin gating)
	Artifacts []string // file paths saved this turn
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
	if cfg.BankrKey != "" {
		defs = append(defs, mk("bankr",
			"Bankr wallet + token agent (Base). Natural-language: create a wallet, check balances/portfolio/prices, launch a token, or trade (send/swap/buy/sell). "+
				"READS (balances, prices, portfolio, fees, token info) run for anyone. "+
				"WRITES that move funds or deploy (create wallet, launch, send, swap, buy, sell, claim) require confirm:true AND an authorized user — "+
				"ALWAYS show the exact action and wait for the human's explicit 'yes' before setting confirm:true; deploys and trades are irreversible.",
			`{"type":"object","properties":{"prompt":{"type":"string","description":"natural-language instruction for the Bankr agent"},"confirm":{"type":"boolean","description":"set true ONLY after the human explicitly approved this specific fund-moving action"}},"required":["prompt"]}`))
	}
	return defs
}

func (tc *ToolCtx) Run(name, args string) string {
	var a struct {
		Query, URL, Name, Content, Note, Prompt string
		Confirm                                 bool
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "tool error: bad arguments: " + err.Error()
	}
	switch name {
	case "web_search":
		return webSearch(a.Query)
	case "fetch_url":
		return fetchURL(a.URL)
	case "save_artifact":
		return tc.saveArtifact(a.Name, a.Content)
	case "remember":
		return appendMemory(tc.cfg, a.Note)
	case "bankr":
		return tc.runBankr(a.Prompt, a.Confirm)
	}
	return "tool error: unknown tool " + name
}

// runBankr enforces the money guardrails BEFORE any request leaves the box:
// fund-moving / deploy prompts need an authorized Discord user and an
// explicit confirm flag. Reads are open. The prompt tells Vela to get a
// human "yes" first; this is the hard backstop under that.
func (tc *ToolCtx) runBankr(prompt string, confirm bool) string {
	if tc.bankr == nil {
		return "bankr is not configured on this instance"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "bankr error: empty prompt"
	}
	if isBankrWrite(prompt) {
		if !tc.cfg.BankrAdmins[tc.authorID] {
			return "REFUSED: this is a fund-moving/deploy action and this user is not on the Bankr admin allowlist. " +
				"Tell them only an authorized admin can execute wallet creation, launches, or trades. Reads (balances, prices, portfolio) are fine for anyone."
		}
		if !confirm {
			return "HOLD: confirmation required. Show the human the EXACT action (amounts, token, recipient, that it is irreversible) " +
				"and only call bankr again with confirm:true after they reply with an explicit yes."
		}
	}
	out, err := tc.bankr.Prompt(prompt)
	if err != nil {
		return "bankr error: " + err.Error()
	}
	return out
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

func getPage(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nanoclaw/1.0)")
	resp, err := httpClient.Do(req)
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
	links := ddgResult.FindAllStringSubmatch(page, 8)
	snips := ddgSnippet.FindAllStringSubmatch(page, 8)
	if len(links) == 0 {
		return "no results"
	}
	var b strings.Builder
	for i, m := range links {
		u := m[1]
		// DDG wraps targets in a redirect: //duckduckgo.com/l/?uddg=<real>
		if p, err := url.Parse(u); err == nil {
			if real := p.Query().Get("uddg"); real != "" {
				u = real
			}
		}
		snippet := ""
		if i < len(snips) {
			snippet = stripTags(snips[i][1])
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, stripTags(m[2]), u, snippet)
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
	text := stripTags(wsRe.ReplaceAllString(page, "\n"))
	if len(text) > 6000 {
		text = text[:6000] + " …[truncated]"
	}
	return text
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func (tc *ToolCtx) saveArtifact(name, content string) string {
	name = unsafeName.ReplaceAllString(filepath.Base(name), "-")
	if name == "" || name == "." {
		return "artifact error: bad name"
	}
	path := filepath.Join(tc.cfg.DataDir, "artifacts",
		fmt.Sprintf("%d-%s", time.Now().Unix(), name))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "artifact error: " + err.Error()
	}
	tc.Artifacts = append(tc.Artifacts, path)
	return fmt.Sprintf("saved %s (%d bytes) — it will be attached to your reply", name, len(content))
}
