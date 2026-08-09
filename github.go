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
	"strings"
)

// github.go — an API-ONLY GitHub tool: create repos, write files, open PRs,
// fork. It never runs anything on the box (that's the coder shell, gated
// separately), so it can be offered to the whole server. All actions use
// Vela's own token and act as her account; every call is audit-logged.

const ghAPI = "https://api.github.com"

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
// the whole server unless NANOCLAW_REPO_USERS narrows it. Every call is logged.
func (tc *ToolCtx) runGithub(a toolArgs) string {
	if !tc.cfg.RepoAllowed(tc.authorID) {
		return "REFUSED: the GitHub tool is limited to an allowlist (NANOCLAW_REPO_USERS) on this server, and this user isn't on it. Tell them plainly."
	}
	gh := newGH(tc.cfg)
	if gh == nil {
		return "github isn't configured on this instance (no token)."
	}
	tc.usedCode = true // only once past the gates — a REFUSED call ran nothing
	log.Printf("github action=%s by=%s repo=%.60q name=%.40q path=%.60q",
		a.Action, tc.authorID, a.Repo, a.Name, a.Path)
	switch a.Action {
	case "create_repo":
		if a.Name == "" {
			return "github error: create_repo needs a name"
		}
		return gh.createRepo(a.Name, a.Description, a.Private)
	case "put_file":
		if a.Repo == "" || a.Path == "" {
			return "github error: put_file needs repo and path"
		}
		return gh.putFile(a.Repo, a.Path, a.Content, a.Message, a.Branch)
	case "open_pr":
		if a.Repo == "" || a.Title == "" || a.Head == "" {
			return "github error: open_pr needs repo ('owner/name'), title, and head"
		}
		return gh.openPR(a.Repo, a.Title, a.Head, a.Base, a.Body)
	case "fork":
		if a.Repo == "" {
			return "github error: fork needs repo ('owner/name')"
		}
		return gh.fork(a.Repo)
	}
	return "github error: unknown action " + a.Action + " (use create_repo|put_file|open_pr|fork)"
}

type ghClient struct {
	token string
	owner string // cached authenticated login (Velaoc)
}

func newGH(cfg *Config) *ghClient {
	if cfg.GitHubToken == "" {
		return nil
	}
	return &ghClient{token: cfg.GitHubToken}
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
	req, err := http.NewRequest(method, ghAPI+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "nanoclaw")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ssrfClient.Do(req)
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

func (g *ghClient) createRepo(name, desc string, private bool) string {
	m, st, err := g.do("POST", "/user/repos", map[string]any{
		"name": name, "description": desc, "private": private, "auto_init": true,
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
	url := ""
	if c, ok := m["content"].(map[string]any); ok {
		url, _ = c["html_url"].(string)
	}
	return fmt.Sprintf("committed %s to %s/%s — %s", path, owner, repo, url)
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
