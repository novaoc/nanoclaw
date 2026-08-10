package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Model-release tracking via the Hugging Face public API. We poll a curated
// set of labs' newest models, remember the ids we've already reported (a small
// file on the SD), and flag which are new since last check — so "any new models
// out?" gives a real, dated answer instead of a guess.

// trackedLabs are the HF orgs worth watching; overridable via NANOCLAW_MODEL_ORGS.
var trackedLabs = []string{
	"deepseek-ai", "Qwen", "meta-llama", "mistralai", "google",
	"openai", "microsoft", "nvidia", "moonshotai", "zai-org",
}

func modelOrgs(cfg *Config) []string {
	if v := strings.TrimSpace(os.Getenv("NANOCLAW_MODEL_ORGS")); v != "" {
		var out []string
		for _, o := range strings.Split(v, ",") {
			if o = strings.TrimSpace(o); o != "" {
				out = append(out, o)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return trackedLabs
}

type hfModel struct {
	ID        string  `json:"id"`        // "deepseek-ai/DeepSeek-V3"
	CreatedAt string  `json:"createdAt"` // RFC3339
	Downloads int     `json:"downloads"` //
	Likes     int     `json:"likes"`     //
	Pipeline  string  `json:"pipeline_tag"`
	Gated     any     `json:"gated"`
	Private   bool    `json:"private"`
	Trending  float64 `json:"trendingScore"`
}

// fetchLabModels pulls a lab's newest models (by creation date).
func fetchLabModels(author string, limit int) []hfModel {
	u := fmt.Sprintf("https://huggingface.co/api/models?author=%s&sort=createdAt&direction=-1&limit=%d&full=false",
		url.QueryEscape(author), limit)
	var out []hfModel
	if err := fetchJSON(u, &out); err != nil {
		return nil
	}
	return out
}

func seenPath(cfg *Config) string { return filepath.Join(cfg.DataDir, "seen-models.txt") }

func loadSeen(cfg *Config) map[string]bool {
	seen := map[string]bool{}
	b, err := os.ReadFile(seenPath(cfg))
	if err != nil {
		return seen
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			seen[l] = true
		}
	}
	return seen
}

func saveSeen(cfg *Config, seen map[string]bool) {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	_ = os.WriteFile(seenPath(cfg), []byte(strings.Join(ids, "\n")+"\n"), 0o644)
}

// modelReleases returns recent models across tracked labs (or a filtered lab/
// name), newest first, marking which are new since the last call.
func (tc *ToolCtx) modelReleases(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))
	labs := modelOrgs(tc.cfg)
	// If the query names a tracked lab, narrow to it and show everything from
	// that lab. Otherwise keep all labs but filter models by name substring.
	labNarrowed := false
	if q != "" {
		var narrow []string
		for _, l := range labs {
			if strings.Contains(strings.ToLower(l), q) {
				narrow = append(narrow, l)
			}
		}
		if len(narrow) > 0 {
			labs, labNarrowed = narrow, true
		}
	}
	var all []hfModel
	for _, lab := range labs {
		for _, m := range fetchLabModels(lab, 12) {
			if m.Private {
				continue
			}
			if q != "" && !labNarrowed && !strings.Contains(strings.ToLower(m.ID), q) {
				continue // free-text query that isn't a lab name → match model id
			}
			all = append(all, m)
		}
	}
	if len(all) == 0 {
		return "No releases came back from Hugging Face just now (API hiccup or nothing matched) — try again shortly."
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })

	seen := loadSeen(tc.cfg)
	var b strings.Builder
	shown := 0
	newCount := 0
	for _, m := range all {
		if shown >= 20 {
			break
		}
		isNew := !seen[m.ID]
		if isNew {
			newCount++
			seen[m.ID] = true
		}
		tag := ""
		if isNew {
			tag = " (NEW)"
		}
		fmt.Fprintf(&b, "%s%s — %s, %s downloads\n", m.ID, tag, prettyDate(m.CreatedAt), thousands(m.Downloads))
		shown++
	}
	saveSeen(tc.cfg, seen)
	head := fmt.Sprintf("Recent model releases (%d new since last check):\n", newCount)
	return clip(head+b.String()+"(dates are HF upload dates. Flag the NEW ones to the user; offer a benchmark chart if they want to compare.)", 6000)
}

func prettyDate(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.UTC().Format("Jan 2 2006")
	}
	if len(rfc) >= 10 {
		return rfc[:10]
	}
	return rfc
}

func thousands(n int) string { return withCommas(fmt.Sprintf("%d", n)) }
