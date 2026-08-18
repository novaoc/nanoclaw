package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// github.go — an API-ONLY GitHub tool: create repos, write files, open PRs,
// fork. It never runs anything on the box (that's the coder shell, gated
// separately), so it can be offered to the whole server. All actions use
// Vela's own token and act as her account; every call is audit-logged.

const defaultGHAPI = "https://api.github.com"

const maxRepoArchive = 96 << 20

// maxRemoteSearchBlobs caps how many blobs one search_code call will fetch
// after path/glob filtering, so a whole-repo scan cannot hammer the API.
const maxRemoteSearchBlobs = 400

var repoPartRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ghEsc escapes a repo path or branch name segment-by-segment (slashes kept),
// so a name containing '#', '?', or spaces can't be reinterpreted as URL
// syntax — "notes#1.md" must write notes#1.md, not silently commit to "notes".
func ghEsc(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// runGithub is the tool entry point: allowlist gate, then dispatch. It runs no
// code on the box — only GitHub API writes as Vela's account — so it's open to
// the whole server unless VELA_REPO_USERS narrows it. Every call is logged.
func (tc *ToolCtx) runGithub(a toolArgs) string {
	if !tc.cfg.RepoAllowed(tc.authorID) {
		return "REFUSED: the GitHub tool is limited to an allowlist (VELA_REPO_USERS) on this server, and this user isn't on it. Tell them plainly."
	}
	gh := tc.gh
	if gh == nil {
		gh = newGH(tc.cfg)
	}
	if gh == nil {
		return "github isn't configured on this instance (no token)."
	}
	log.Printf("github action=%s by=%s repo=%.60q name=%.40q path=%.60q",
		a.Action, tc.authorID, a.Repo, a.Name, a.Path)
	switch a.Action {
	case "create_repo":
		if tc.cfg.RailsTemplate != "" {
			return "github error: Vela applications are Rails-only on this instance; use create_rails_app so the private production framework and Material Design contract are preserved"
		}
		if a.Name == "" {
			return "github error: create_repo needs a name"
		}
		tc.usedCode = true
		return gh.createRepo(a.Name, a.Description)
	case "create_rails_app":
		if a.Name == "" {
			return "github error: create_rails_app needs a name"
		}
		if tc.cfg.RailsTemplate == "" {
			return "the private Vela Rails foundation isn't configured on this instance"
		}
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		out := gh.createFromTemplate(tc.cfg.RailsTemplate, a.Name, a.Description, tc.cfg.PublicApps)
		if !strings.HasPrefix(out, "created Rails app") {
			return out
		}
		// One interim channel message per successful scaffold — not on failure,
		// not on ordinary tool calls. Time bound comes from the real turn deadline.
		tc.notifyBuildStarted(a.Name)
		// Shaping is mechanical and must not depend on the model calling a
		// separate step: stamp identity, write app README, omit unselected modules.
		shapeOut := gh.shapeGeneratedApp(a.Name, tc.appSpec, tc.cfg)
		// When the build worker is configured, the hand-off is not optional.
		// Without this line the model sometimes built inline out of habit
		// (2026-08-18: a whole roguelike written file-by-file on the board
		// while the worker sat idle) — the tool result is the strongest
		// steering surface we have, so the routing lives here.
		if tc.cfg.WorkerEnabled() {
			return out + "\n" + shapeOut + "\n\nNEXT STEP (required on this instance): call enqueue_build with this repo, the app name, and the request context. Do NOT implement the product yourself with put_file/patch_file, and do NOT call verify_repo — the worker codes, tests, and verifies; the deploy is automatic after it passes. Your only remaining job here is enqueue_build, then tell the requester the build is underway."
		}
		return out + "\n" + shapeOut
	case "shape":
		// Optional re-shape of an existing generated app (identity/README/modules).
		// create_rails_app already shapes; nothing depends on calling this.
		if a.Repo == "" {
			return "github error: shape needs repo"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.shapeGeneratedApp(a.Repo, tc.appSpec, tc.cfg)
	case "publish_app":
		if a.Repo == "" {
			return "github error: publish_app needs repo"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		tc.usedCode = true
		return gh.publishApp(a.Repo, tc.cfg.PublicApps)
	case "search_code":
		if a.Repo == "" || strings.TrimSpace(a.Pattern) == "" {
			return "github error: search_code needs repo and pattern"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		if tc.repoReads >= 12 {
			return "INSPECTION_COMPLETE: repository read limit reached. You already have enough context; stop inspecting and make the focused patch_file/put_file changes now."
		}
		tc.repoReads++
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.searchCode(a.Repo, a.Pattern, a.Path, a.Glob, a.Ref)
	case "list_tree":
		if a.Repo == "" {
			return "github error: list_tree needs repo"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		if tc.repoReads >= 12 {
			return "INSPECTION_COMPLETE: repository read limit reached. You already have enough context; stop inspecting and make the focused patch_file/put_file changes now."
		}
		tc.repoReads++
		tc.usedCode = true
		return gh.listTree(a.Repo, a.Ref, a.Path)
	case "read_files":
		if a.Repo == "" || len(a.Paths) == 0 || len(a.Paths) > 3 {
			return "github error: read_files needs repo and 1-3 paths"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		if tc.repoReads >= 12 {
			return "INSPECTION_COMPLETE: repository read limit reached. You already have enough context; stop inspecting and make the focused patch_file/put_file changes now."
		}
		tc.repoReads++
		tc.usedCode = true
		return gh.readFiles(a.Repo, a.Ref, a.Paths, a.StartLine, a.EndLine)
	case "describe_schema":
		if a.Repo == "" {
			return "github error: describe_schema needs repo"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		if tc.repoReads >= 12 {
			return "INSPECTION_COMPLETE: repository read limit reached. You already have enough context; stop inspecting and make the focused patch_file/put_file changes now."
		}
		tc.repoReads++
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.describeSchema(a.Repo, a.Ref, a.Tables)
	case "patch_file":
		if a.Repo == "" || a.Path == "" {
			return "github error: patch_file needs repo and path"
		}
		if len(a.Ops) == 0 {
			return "github error: patch_file needs a non-empty ops array"
		}
		if why := refuseGeneratedArtifact(a.Path); why != "" {
			return "github error: " + why
		}
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.patchFile(a.Repo, a.Path, a.Ops, a.Message, a.Branch)
	case "put_file":
		if a.Repo == "" || a.Path == "" {
			return "github error: put_file needs repo and path"
		}
		if why := refuseGeneratedArtifact(a.Path); why != "" {
			return "github error: " + why
		}
		if why := refuseRuntimeSchemaMutation(a.Path, a.Content); why != "" {
			return "github error: " + why
		}
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.putFile(a.Repo, a.Path, a.Content, a.Message, a.Branch)
	case "delete_file":
		if a.Repo == "" || a.Path == "" {
			return "github error: delete_file needs repo and path"
		}
		if err := gh.requireOwnedRepo(a.Repo); err != nil {
			return "github error: " + err.Error()
		}
		tc.usedCode = true
		tc.ensureGHCache()
		gh.cache = tc.ghCache
		return gh.deleteFile(a.Repo, a.Path, a.Message, a.Branch)
	case "open_pr":
		if a.Repo == "" || a.Title == "" || a.Head == "" {
			return "github error: open_pr needs repo ('owner/name'), title, and head"
		}
		tc.usedCode = true
		return gh.openPR(a.Repo, a.Title, a.Head, a.Base, a.Body)
	case "fork":
		if a.Repo == "" {
			return "github error: fork needs repo ('owner/name')"
		}
		tc.usedCode = true
		return gh.fork(a.Repo)
	case "enable_pages":
		if a.Repo == "" {
			return "github error: enable_pages needs repo"
		}
		tc.usedCode = true
		return gh.enablePages(a.Repo)
	}
	return "github error: unknown action " + a.Action + " (use create_repo|create_rails_app|shape|publish_app|search_code|list_tree|read_files|describe_schema|patch_file|put_file|delete_file|open_pr|fork|enable_pages)"
}

// ghRepoCache holds git trees and blobs for the current tool turn so remote
// search_code does not re-fetch the same tree/blob on every call. Lifetime is
// the ToolCtx (one agent turn / code-budget continuation). Writes invalidate
// the affected owner/name entries.
type ghRepoCache struct {
	trees map[string][]ghTreeBlob // "owner/name@ref" → blob entries
	blobs map[string][]byte       // blob sha → content
}

type ghTreeBlob struct {
	Path string
	SHA  string
	Size int64
}

func (tc *ToolCtx) ensureGHCache() {
	if tc.ghCache == nil {
		tc.ghCache = &ghRepoCache{
			trees: map[string][]ghTreeBlob{},
			blobs: map[string][]byte{},
		}
	}
}

func (c *ghRepoCache) invalidateRepo(owner, name string) {
	if c == nil {
		return
	}
	prefix := owner + "/" + name + "@"
	for k := range c.trees {
		if strings.HasPrefix(k, prefix) {
			delete(c.trees, k)
		}
	}
	// Blobs are content-addressed; stale SHAs simply go unused. Clear them if
	// the map grows large so a long build cannot retain unbounded memory.
	if len(c.blobs) > 512 {
		c.blobs = map[string][]byte{}
	}
}

type ghClient struct {
	token  string
	owner  string // cached authenticated login (Velaoc)
	api    string // empty → defaultGHAPI (tests point at httptest)
	client *http.Client // nil → ssrfClient
	cache  *ghRepoCache
}

func newGH(cfg *Config) *ghClient {
	if cfg.GitHubToken == "" {
		return nil
	}
	return &ghClient{token: cfg.GitHubToken}
}

func (g *ghClient) apiBase() string {
	if g.api != "" {
		return g.api
	}
	return defaultGHAPI
}

func (g *ghClient) http() *http.Client {
	if g.client != nil {
		return g.client
	}
	return ssrfClient
}

// do issues an API call and returns the decoded JSON, the status, and any
// transport error. Bodies are capped; we never stream unbounded responses.
func (g *ghClient) do(method, path string, body any) (map[string]any, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.apiBase()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "vela")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out, resp.StatusCode, nil
}

func ghErr(m map[string]any, status int) string {
	if m != nil {
		if msg, ok := m["message"].(string); ok {
			return fmt.Sprintf("GitHub %d: %s", status, msg)
		}
	}
	return fmt.Sprintf("GitHub %d", status)
}

func (g *ghClient) login() (string, error) {
	if g.owner != "" {
		return g.owner, nil
	}
	m, st, err := g.do("GET", "/user", nil)
	if err != nil {
		return "", err
	}
	if st >= 400 {
		return "", errors.New(ghErr(m, st))
	}
	login, _ := m["login"].(string)
	if login == "" {
		return "", fmt.Errorf("could not resolve the token's account")
	}
	g.owner = login
	return login, nil
}

func (g *ghClient) resolveRepo(repo string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	owner := ""
	name := repo
	if strings.Contains(repo, "/") {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 {
			return "", "", fmt.Errorf("repo must be 'name' or 'owner/name'")
		}
		owner, name = parts[0], parts[1]
	} else {
		var err error
		owner, err = g.login()
		if err != nil {
			return "", "", err
		}
	}
	if !repoPartRe.MatchString(owner) || !repoPartRe.MatchString(name) {
		return "", "", fmt.Errorf("invalid repository name")
	}
	return owner, name, nil
}

func (g *ghClient) requireOwnedRepo(repo string) error {
	owner, _, err := g.resolveRepo(repo)
	if err != nil {
		return err
	}
	login, err := g.login()
	if err != nil {
		return err
	}
	if !strings.EqualFold(owner, login) {
		return errors.New("repository inspection is limited to Vela's own repos")
	}
	return nil
}

func (g *ghClient) defaultRef(owner, repo, ref string) (string, error) {
	if strings.TrimSpace(ref) != "" {
		return strings.TrimSpace(ref), nil
	}
	info, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return "", err
	}
	if st >= 400 {
		return "", errors.New(ghErr(info, st))
	}
	ref, _ = info["default_branch"].(string)
	if ref == "" {
		ref = "main"
	}
	return ref, nil
}

func (g *ghClient) listTree(repo, ref, prefix string) string {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "github error: " + err.Error()
	}
	ref, err = g.defaultRef(owner, name, ref)
	if err != nil {
		return "github error: " + err.Error()
	}
	m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", owner, name, url.PathEscape(ref)), nil)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't inspect tree — " + ghErr(m, st)
	}
	prefix = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(prefix)), "/")
	var paths []string
	if entries, ok := m["tree"].([]any); ok {
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			path, _ := entry["path"].(string)
			typ, _ := entry["type"].(string)
			if typ == "blob" && (prefix == "" || strings.HasPrefix(path, prefix)) {
				paths = append(paths, path)
				if len(paths) == 300 {
					break
				}
			}
		}
	}
	if len(paths) == 0 {
		return "no files found for that repository/prefix"
	}
	return fmt.Sprintf("repository tree at %s (%d files shown):\n%s", ref, len(paths), strings.Join(paths, "\n"))
}

// describeSchema returns a compact table/column/index/FK summary parsed from
// db/schema.rb at ref, via the turn-scoped tree/blob cache. Empty tables lists
// names only. Prefer this over read_files on db/schema.rb.
func (g *ghClient) describeSchema(repo, ref string, tables []string) string {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "github error: " + err.Error()
	}
	ref, err = g.defaultRef(owner, name, ref)
	if err != nil {
		return "github error: " + err.Error()
	}
	raw, err := g.fetchRepoSchemaRB(owner, name, ref)
	if err != nil {
		return err.Error()
	}
	schema, err := parseSchemaRB(string(raw))
	if err != nil {
		return err.Error()
	}
	return formatSchemaInspect(schema, normalizeTableArgs(tables))
}

// fetchRepoSchemaRB loads db/schema.rb through cachedTree + cachedBlob.
func (g *ghClient) fetchRepoSchemaRB(owner, name, ref string) ([]byte, error) {
	entries, err := g.cachedTree(owner, name, ref)
	if err != nil {
		return nil, fmt.Errorf("github error: %s", err.Error())
	}
	var sha string
	for _, e := range entries {
		if e.Path == "db/schema.rb" {
			sha = e.SHA
			break
		}
	}
	if sha == "" {
		return nil, fmt.Errorf("db/schema.rb is missing from this repository")
	}
	raw, err := g.cachedBlob(owner, name, sha)
	if err != nil {
		return nil, fmt.Errorf("db/schema.rb could not be read: %s", err.Error())
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("db/schema.rb is empty")
	}
	return raw, nil
}

func (g *ghClient) readFiles(repo, ref string, paths []string, startLine, endLine int) string {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "github error: " + err.Error()
	}
	ref, err = g.defaultRef(owner, name, ref)
	if err != nil {
		return "github error: " + err.Error()
	}
	budget := readBudgetSingle
	if len(paths) > 1 {
		budget = readBudgetMulti
	}
	var out strings.Builder
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" || len(path) > 250 || strings.HasPrefix(path, "/") ||
			filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsRune(path, 0) {
			fmt.Fprintf(&out, "=== %s ===\nERROR: invalid path\n", path)
			continue
		}
		m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, name, ghEsc(path), url.QueryEscape(ref)), nil)
		if err != nil || st >= 400 {
			fmt.Fprintf(&out, "=== %s ===\nERROR: %s\n", path, ghErr(m, st))
			continue
		}
		encoded, _ := m["content"].(string)
		content, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", ""))
		if decodeErr != nil {
			fmt.Fprintf(&out, "=== %s ===\nERROR: couldn't decode file\n", path)
			continue
		}
		p := path
		sl, el := startLine, endLine
		body := readFileWindow(string(content), sl, el, budget, func(next int) string {
			if el > 0 {
				return fmt.Sprintf(`read_files paths=["%s"] start_line=%d end_line=%d`, p, next, el)
			}
			return fmt.Sprintf(`read_files paths=["%s"] start_line=%d`, p, next)
		})
		fmt.Fprintf(&out, "=== %s ===\n%s\n", path, body)
	}
	return strings.TrimSpace(out.String())
}

// searchCode regex-searches a remote repo by walking the git tree and fetching
// blobs (not GitHub code-search — that lags on brand-new repos). Matching and
// output formatting reuse the local search helpers so the model sees one shape.
func (g *ghClient) searchCode(repo, pattern, pathPrefix, glob, ref string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "search error: pattern is required"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "search error: bad pattern: " + err.Error()
	}
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "github error: " + err.Error()
	}
	ref, err = g.defaultRef(owner, name, ref)
	if err != nil {
		return "github error: " + err.Error()
	}
	entries, err := g.cachedTree(owner, name, ref)
	if err != nil {
		return "github error: " + err.Error()
	}

	var b strings.Builder
	matches := 0
	truncated := false
	blobsFetched := 0
	for _, e := range entries {
		if truncated {
			break
		}
		if searchSkipRemotePath(e.Path) {
			continue
		}
		if !searchPathPrefixOK(e.Path, pathPrefix) {
			continue
		}
		if !searchGlobOK(e.Path, glob) {
			continue
		}
		if e.Size > maxSearchFileBytes {
			continue
		}
		if blobsFetched >= maxRemoteSearchBlobs {
			truncated = true
			break
		}
		raw, err := g.cachedBlob(owner, name, e.SHA)
		if err != nil {
			continue // skip unreadable blobs; keep searching
		}
		blobsFetched++
		n, hitTrunc := appendContentSearchHits(&b, e.Path, raw, re, maxSearchResults-matches)
		matches += n
		if hitTrunc || matches >= maxSearchResults {
			truncated = true
		}
	}
	return finishSearchOutput(&b, matches, truncated)
}

func (g *ghClient) cachedTree(owner, name, ref string) ([]ghTreeBlob, error) {
	key := owner + "/" + name + "@" + ref
	if g.cache != nil {
		if entries, ok := g.cache.trees[key]; ok {
			return entries, nil
		}
	}
	m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", owner, name, url.PathEscape(ref)), nil)
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, errors.New(ghErr(m, st))
	}
	var entries []ghTreeBlob
	if tree, ok := m["tree"].([]any); ok {
		for _, raw := range tree {
			entry, _ := raw.(map[string]any)
			typ, _ := entry["type"].(string)
			if typ != "blob" {
				continue
			}
			path, _ := entry["path"].(string)
			sha, _ := entry["sha"].(string)
			if path == "" || sha == "" {
				continue
			}
			var size int64
			switch v := entry["size"].(type) {
			case float64:
				size = int64(v)
			case json.Number:
				size, _ = v.Int64()
			}
			entries = append(entries, ghTreeBlob{Path: path, SHA: sha, Size: size})
		}
	}
	if g.cache != nil {
		g.cache.trees[key] = entries
	}
	return entries, nil
}

func (g *ghClient) cachedBlob(owner, name, sha string) ([]byte, error) {
	if g.cache != nil {
		if raw, ok := g.cache.blobs[sha]; ok {
			return raw, nil
		}
	}
	m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/git/blobs/%s", owner, name, url.PathEscape(sha)), nil)
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, errors.New(ghErr(m, st))
	}
	encoded, _ := m["content"].(string)
	if encoded == "" {
		return nil, fmt.Errorf("empty blob")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", ""))
	if err != nil {
		return nil, err
	}
	if g.cache != nil {
		g.cache.blobs[sha] = raw
	}
	return raw, nil
}

// patchFile reads one remote file, applies applyOps (all-or-nothing), and
// commits only when every op succeeds. Returns the compact +/− summary.
func (g *ghClient) patchFile(repo, path string, ops []patchOp, message, branch string) string {
	owner, err := g.login()
	if err != nil {
		return "github error: " + err.Error()
	}
	name := repo
	if strings.Contains(repo, "/") {
		parts := strings.SplitN(repo, "/", 2)
		owner, name = parts[0], parts[1]
	}
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || len(path) > 250 || strings.HasPrefix(path, "/") ||
		filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsRune(path, 0) {
		return "github error: invalid path"
	}
	if err := g.ensureBranch(owner, name, branch); err != nil {
		return "couldn't prepare branch: " + err.Error()
	}
	q := ""
	if branch != "" {
		q = "?ref=" + url.QueryEscape(branch)
	}
	cur, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s%s", owner, name, ghEsc(path), q), nil)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "patch error: " + ghErr(cur, st)
	}
	sha, _ := cur["sha"].(string)
	encoded, _ := cur["content"].(string)
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", ""))
	if err != nil {
		return "patch error: couldn't decode file"
	}
	old := string(raw)
	next, err := applyOps(old, ops)
	if err != nil {
		return "patch error: " + err.Error()
	}
	if why := refuseRuntimeSchemaMutation(path, next); why != "" {
		return "patch error: " + why
	}
	if why := g.refuseMigrationWrite(owner, name, branch, path); why != "" {
		return "patch error: " + why
	}
	if next == old {
		return fmt.Sprintf("patched %s: %d ops (+0 -0 lines) — content unchanged", path, len(ops))
	}
	if message == "" {
		message = "patch " + path
	}
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(next)),
		"sha":     sha,
	}
	if branch != "" {
		payload["branch"] = branch
	}
	m, st, err := g.do("PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, name, ghEsc(path)), payload)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't patch file — " + ghErr(m, st)
	}
	if g.cache != nil {
		g.cache.invalidateRepo(owner, name)
	}
	add, del := lineDiffCounts(old, next)
	return fmt.Sprintf("patched %s: %d ops (+%d -%d lines)", path, len(ops), add, del)
}

// downloadArchive resolves ref to an immutable commit SHA, then downloads the
// repository tarball to a 0600 temporary file. The GitHub token stays in the
// request header and never enters a URL, command line, tool result, or log.
func (g *ghClient) downloadArchive(repo, ref, tempDir string) (string, string, error) {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "", "", err
	}
	if ref == "" {
		info, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", owner, name), nil)
		if err != nil {
			return "", "", err
		}
		if st >= 400 {
			return "", "", errors.New(ghErr(info, st))
		}
		ref, _ = info["default_branch"].(string)
		if ref == "" {
			ref = "main"
		}
	}
	commit, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/commits/%s", owner, name, url.PathEscape(ref)), nil)
	if err != nil {
		return "", "", err
	}
	if st >= 400 {
		return "", "", errors.New(ghErr(commit, st))
	}
	sha, _ := commit["sha"].(string)
	if len(sha) != 40 {
		return "", "", fmt.Errorf("GitHub did not resolve %s to a commit", ref)
	}

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/repos/%s/%s/tarball/%s", g.apiBase(), owner, name, sha), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "vela")
	client := *g.http()
	client.Timeout = 2 * time.Minute
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("GitHub %d: %.1000s", resp.StatusCode, raw)
	}
	if resp.ContentLength > maxRepoArchive {
		return "", "", fmt.Errorf("repository archive exceeds %dMB", maxRepoArchive>>20)
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(tempDir, "repo-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	path := filepath.Clean(f.Name())
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxRepoArchive+1))
	if err != nil {
		return "", "", err
	}
	if n == 0 || n > maxRepoArchive {
		return "", "", fmt.Errorf("repository archive is empty or exceeds %dMB", maxRepoArchive>>20)
	}
	if err := f.Sync(); err != nil {
		return "", "", err
	}
	ok = true
	return path, sha, nil
}

// createRepo creates public repositories for work Vela makes on request. The
// private foundation is managed separately and never goes through this action.
func (g *ghClient) createRepo(name, desc string) string {
	m, st, err := g.do("POST", "/user/repos", map[string]any{
		"name": name, "description": desc, "private": false, "auto_init": true,
	})
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't create repo — " + ghErr(m, st)
	}
	url, _ := m["html_url"].(string)
	return fmt.Sprintf("created repo %s", url)
}

// createFromTemplate creates a repository from Vela's Rails foundation. The
// configured template name stays out of the model prompt and result; callers
// only need to choose the new application's name.
//
// Visibility follows the operator's setting. Applications are created public
// when publication is enabled (VELA_PUBLIC_APPS=1) and private otherwise.
//
// The private default existed because the foundation was a derivative of
// commercially licensed code: a public repository generated from it would have
// distributed that code from its first instant. The foundation is now an
// original MIT rewrite that is itself public, so that reason is gone — but the
// gate is kept rather than deleted, because an instance pointed at a private
// or derivative template must still start private.
func (g *ghClient) createFromTemplate(template, name, desc string, public bool) string {
	owner, repo, err := g.resolveRepo(template)
	if err != nil {
		return "couldn't resolve the Rails foundation — " + err.Error()
	}
	destinationOwner, err := g.login()
	if err != nil {
		return "github error: " + err.Error()
	}
	m, st, err := g.do("POST", fmt.Sprintf("/repos/%s/%s/generate", owner, repo), map[string]any{
		"owner": destinationOwner, "name": name, "description": desc,
		"private": !public, "include_all_branches": false,
	})
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't create Rails app — " + ghErr(m, st)
	}
	u, _ := m["html_url"].(string)
	if public {
		return fmt.Sprintf("created Rails app %s from Vela's production foundation — public and forkable. Shaping identity/README/modules next.", u)
	}
	return fmt.Sprintf("created Rails app %s from Vela's production foundation (private on this instance). Shaping identity/README/modules next; after verify, use publish_app to make it public and forkable.", u)
}

// publishApp makes a generated application public once it carries its own
// identity. It is refused unless the operator has turned publication on, so a
// containment period — such as an unreleased foundation — cannot be ended by
// the model on its own.
func (g *ghClient) publishApp(repoFull string, enabled bool) string {
	if !enabled {
		return "publishing is turned off on this instance (VELA_PUBLIC_APPS is not 1), so the app stays private. Tell the requester it's ready and that the operator can publish it."
	}
	owner, repo, err := g.resolveRepo(repoFull)
	if err != nil {
		return "github error: " + err.Error()
	}
	m, st, err := g.do("PATCH", fmt.Sprintf("/repos/%s/%s", owner, repo), map[string]any{
		"private": false,
	})
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't publish the app — " + ghErr(m, st)
	}
	u, _ := m["html_url"].(string)
	return fmt.Sprintf("published %s — it's public and forkable now", u)
}

func (g *ghClient) fork(repoFull string) string {
	m, st, err := g.do("POST", "/repos/"+repoFull+"/forks", nil)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't fork — " + ghErr(m, st)
	}
	url, _ := m["html_url"].(string)
	return fmt.Sprintf("forked to %s (may take a few seconds to be ready)", url)
}

// ensureBranch makes sure branch exists in owner/repo, creating it off the
// default branch when missing, so putFile + openPR can build a PR head.
func (g *ghClient) ensureBranch(owner, repo, branch string) error {
	if branch == "" {
		return nil
	}
	if _, st, _ := g.do("GET", fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, ghEsc(branch)), nil); st == 200 {
		return nil
	}
	info, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return err
	}
	if st >= 400 {
		return errors.New(ghErr(info, st))
	}
	def, _ := info["default_branch"].(string)
	ref, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, ghEsc(def)), nil)
	if err != nil {
		return err
	}
	if st >= 400 {
		return errors.New(ghErr(ref, st))
	}
	sha := ""
	if obj, ok := ref["object"].(map[string]any); ok {
		sha, _ = obj["sha"].(string)
	}
	m, st, err := g.do("POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo),
		map[string]any{"ref": "refs/heads/" + branch, "sha": sha})
	if err != nil {
		return err
	}
	if st >= 400 {
		return errors.New(ghErr(m, st))
	}
	return nil
}

// refuseMigrationWrite reads the real schema stamp and db/migrate/ siblings
// from the remote repo and refuses a stale migration timestamp.
func (g *ghClient) refuseMigrationWrite(owner, name, branch, filePath string) string {
	proposed, ok := migrationVersionFromPath(filePath)
	if !ok {
		return ""
	}
	ref, err := g.defaultRef(owner, name, branch)
	if err != nil {
		// Cannot read bounds — fail open only for non-migration paths (already
		// filtered); for migrations, treat missing bounds as floor 0 so a
		// well-formed later timestamp still lands.
		return refuseStaleMigration(proposed, 0, 0)
	}
	stamp, maxOther := g.remoteMigrationBounds(owner, name, ref, filePath)
	return refuseStaleMigration(proposed, stamp, maxOther)
}

func (g *ghClient) remoteMigrationBounds(owner, name, ref, selfPath string) (stamp, maxOther int64) {
	q := "?ref=" + url.QueryEscape(ref)
	if m, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s%s", owner, name, ghEsc("db/schema.rb"), q), nil); err == nil && st == 200 {
		if encoded, _ := m["content"].(string); encoded != "" {
			if raw, decErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(encoded, "\n", "")); decErr == nil {
				if v, ok := schemaVersionFromContent(string(raw)); ok {
					stamp = v
				}
			}
		}
	}
	entries, err := g.cachedTree(owner, name, ref)
	if err != nil {
		return stamp, 0
	}
	skip := path.Base(cleanRepoPath(selfPath))
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "db/migrate/") && strings.HasSuffix(e.Path, ".rb") {
			names = append(names, path.Base(e.Path))
		}
	}
	return stamp, maxMigrationVersion(names, skip)
}

// putFile creates or updates a file (a commit) in one of Vela's repos. repo may
// be "name" (her own) or "owner/name". branch is optional (default branch);
// a missing branch is created so this composes with openPR.
func (g *ghClient) putFile(repo, path, content, message, branch string) string {
	owner, err := g.login()
	if err != nil {
		return "github error: " + err.Error()
	}
	if strings.Contains(repo, "/") {
		parts := strings.SplitN(repo, "/", 2)
		owner, repo = parts[0], parts[1]
	}
	if why := g.refuseMigrationWrite(owner, repo, branch, path); why != "" {
		return "github error: " + why
	}
	if err := g.ensureBranch(owner, repo, branch); err != nil {
		return "couldn't prepare branch: " + err.Error()
	}
	// existing file? need its sha to update
	q := ""
	if branch != "" {
		q = "?ref=" + url.QueryEscape(branch)
	}
	sha := ""
	if cur, st, _ := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s%s", owner, repo, ghEsc(path), q), nil); st == 200 {
		sha, _ = cur["sha"].(string)
	}
	if message == "" {
		message = "add " + path
	}
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if branch != "" {
		payload["branch"] = branch
	}
	if sha != "" {
		payload["sha"] = sha
	}
	m, st, err := g.do("PUT", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, ghEsc(path)), payload)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't write file — " + ghErr(m, st)
	}
	if g.cache != nil {
		g.cache.invalidateRepo(owner, repo)
	}
	url := ""
	if c, ok := m["content"].(map[string]any); ok {
		url, _ = c["html_url"].(string)
	}
	return fmt.Sprintf("committed %s to %s/%s — %s", path, owner, repo, url)
}

// deleteFile removes one known file from a repository owned by Vela. It uses
// GitHub's contents API so the model never needs to construct authenticated
// curl commands in the shell.
func (g *ghClient) deleteFile(repo, path, message, branch string) string {
	owner, name, err := g.resolveRepo(repo)
	if err != nil {
		return "github error: " + err.Error()
	}
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || len(path) > 250 || strings.HasPrefix(path, "/") ||
		filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsRune(path, 0) {
		return "github error: invalid path"
	}
	q := ""
	if branch != "" {
		q = "?ref=" + url.QueryEscape(branch)
	}
	current, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s/contents/%s%s", owner, name, ghEsc(path), q), nil)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't find file to delete — " + ghErr(current, st)
	}
	sha, _ := current["sha"].(string)
	if sha == "" {
		return "github error: GitHub did not return the file SHA"
	}
	if message == "" {
		message = "remove " + path
	}
	payload := map[string]any{"message": message, "sha": sha}
	if branch != "" {
		payload["branch"] = branch
	}
	result, st, err := g.do("DELETE", fmt.Sprintf("/repos/%s/%s/contents/%s", owner, name, ghEsc(path)), payload)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't delete file — " + ghErr(result, st)
	}
	if g.cache != nil {
		g.cache.invalidateRepo(owner, name)
	}
	return fmt.Sprintf("deleted %s from %s/%s", path, owner, name)
}

// enablePages turns on GitHub Pages for a repo (deploy-from-branch on the
// default branch root) and returns the live site URL — the publish step after
// create_repo + put_file(index.html). Idempotent: already-enabled reports the
// existing URL.
func (g *ghClient) enablePages(repo string) string {
	owner, err := g.login()
	if err != nil {
		return "github error: " + err.Error()
	}
	if strings.Contains(repo, "/") {
		parts := strings.SplitN(repo, "/", 2)
		owner, repo = parts[0], parts[1]
	}
	info, st, err := g.do("GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't read repo — " + ghErr(info, st)
	}
	branch, _ := info["default_branch"].(string)
	if branch == "" {
		branch = "main"
	}
	m, st, err := g.do("POST", fmt.Sprintf("/repos/%s/%s/pages", owner, repo),
		map[string]any{"source": map[string]any{"branch": branch, "path": "/"}})
	if err != nil {
		return "github error: " + err.Error()
	}
	if st == 409 { // already enabled — fetch and report the live URL
		cur, st2, _ := g.do("GET", fmt.Sprintf("/repos/%s/%s/pages", owner, repo), nil)
		if st2 == 200 {
			if u, _ := cur["html_url"].(string); u != "" {
				return "Pages was already on — the site is live at " + u
			}
		}
	}
	if st >= 400 {
		hint := ""
		if st == 403 || st == 404 {
			hint = " (the GitHub token may lack the Pages write permission, or the repo is private — Pages on private repos needs a paid plan)"
		}
		return "couldn't enable Pages — " + ghErr(m, st) + hint
	}
	u, _ := m["html_url"].(string)
	if u == "" {
		u = fmt.Sprintf("https://%s.github.io/%s/", owner, repo)
	}
	return fmt.Sprintf("Pages enabled — the site will be live at %s in about a minute (index.html at the repo root is the homepage)", u)
}

// openPR opens a pull request on repoFull ("owner/name"). head is "branch" for
// same-repo, or "forkowner:branch" for a cross-repo PR from a fork.
func (g *ghClient) openPR(repoFull, title, head, base, body string) string {
	if base == "" {
		base = "main"
	}
	m, st, err := g.do("POST", "/repos/"+repoFull+"/pulls",
		map[string]any{"title": title, "head": head, "base": base, "body": body})
	if err != nil {
		return "github error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't open PR — " + ghErr(m, st)
	}
	url, _ := m["html_url"].(string)
	return fmt.Sprintf("opened PR %s", url)
}
