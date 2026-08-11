package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Code capability: a shell + file tools so Vela can write code, install
// libraries, and push to her own GitHub. HIGH TRUST — a shell can read
// everything the process can (env, secrets), so it's gated to an explicit
// coder allowlist (VELA_CODERS). Empty allowlist = the capability is off.

func (tc *ToolCtx) isCoder() bool { return tc.cfg.Coders[tc.authorID] }

const coderOnly = "REFUSED: running code/shell is limited to the coder allowlist (VELA_CODERS). " +
	"Tell this user they're not authorized to run commands on the box."

// codeGate is the single authorization point for all code tools: the coder
// allowlist.
func (tc *ToolCtx) codeGate() string {
	if !tc.isCoder() {
		return coderOnly
	}
	return ""
}

// resolveWorkspace confines a path to the workspace and REJECTS (not
// silently rewrites) anything absolute or containing a ".." that climbs out.
func (tc *ToolCtx) resolveWorkspace(p string) (string, error) {
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	ws := tc.cfg.Workspace
	full := filepath.Join(ws, p)
	rel, err := filepath.Rel(ws, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return full, nil
}

// runShell executes a command in the workspace with a hard timeout and a
// capped, rune-safe output. No shell sandboxing beyond the allowlist — this
// is a trusted-coder tool by design.
func (tc *ToolCtx) runShell(command string) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	tc.usedCode = true
	command = strings.TrimSpace(command)
	if command == "" {
		return "shell error: empty command"
	}
	if err := os.MkdirAll(tc.cfg.Workspace, 0o755); err != nil {
		return "shell error: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = tc.cfg.Workspace
	// Inject submitted deploy secrets as env vars (by name) so a command can use
	// $HETZNER_TOKEN etc. without the value ever entering the model's context.
	cmd.Env = append(os.Environ(), tc.cfg.Secrets.EnvPairs()...)
	out, err := cmd.CombinedOutput()
	res := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		res += "\n[timed out after 180s — heavy builds may exceed the board's 256MB/one-core budget; prefer CI]"
	} else if err != nil {
		res += "\n[exit: " + err.Error() + "]"
	}
	if strings.TrimSpace(res) == "" {
		res = "(no output)"
	}
	return clip(res, 6000)
}

func (tc *ToolCtx) writeWorkspaceFile(path, content string) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	tc.usedCode = true
	full, err := tc.resolveWorkspace(path)
	if err != nil {
		return "write error: " + err.Error()
	}
	old := ""
	if b, err := os.ReadFile(full); err == nil {
		old = string(b)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "write error: " + err.Error()
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "write error: " + err.Error()
	}
	add, del := lineDiffCounts(old, content)
	return fmt.Sprintf("wrote %s (%d bytes, +%d -%d lines)", path, len(content), add, del)
}

// File-read budgets (content bytes). Outer agent clip is toolResultBudget.
// Single-file fits ordinary sources (e.g. routes.rb ~9k); multi-file is per path
// so a large first file cannot starve the others.
const (
	readBudgetSingle = 12000
	readBudgetMulti  = 4500
	toolResultBudget = 16000
)

// splitFileLines splits file content into lines without a trailing empty element
// from a final newline (so "a\nb\n" → ["a","b"]).
func splitFileLines(content string) []string {
	if content == "" {
		return nil
	}
	trim := strings.TrimSuffix(content, "\n")
	if trim == "" {
		return []string{""} // content was "\n"
	}
	return strings.Split(trim, "\n")
}

func clipRaw(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// readFileWindow returns a line range of content under a character budget.
// startLine/endLine are 1-based inclusive; 0 means omitted (whole file / to EOF).
// On truncation, appends an honest note with total lines and continueHint(nextStart).
func readFileWindow(content string, startLine, endLine, budget int, continueHint func(nextStart int) string) string {
	if budget <= 0 {
		budget = readBudgetSingle
	}
	lines := splitFileLines(content)
	total := len(lines)

	if startLine < 0 || endLine < 0 {
		return "read error: start_line and end_line must be >= 1 when set"
	}
	if total == 0 {
		if startLine > 0 {
			return fmt.Sprintf("read error: start_line %d is past end of file (0 lines)", startLine)
		}
		return ""
	}

	start := 1
	if startLine > 0 {
		start = startLine
	}
	end := total
	if endLine > 0 {
		end = endLine
	}
	if start > total {
		return fmt.Sprintf("read error: start_line %d is past end of file (%d lines)", start, total)
	}
	if end < start {
		return fmt.Sprintf("read error: end_line %d is before start_line %d", end, start)
	}
	if end > total {
		end = total
	}

	var b strings.Builder
	shownEnd := start - 1
	partialLine := false
	for i := start; i <= end; i++ {
		line := lines[i-1]
		prefix := ""
		if i > start {
			prefix = "\n"
		}
		need := len(prefix) + len(line)
		if b.Len()+need <= budget {
			b.WriteString(prefix)
			b.WriteString(line)
			shownEnd = i
			continue
		}
		if b.Len() == 0 {
			// Single line larger than budget: rune-safe prefix of that line.
			b.WriteString(prefix)
			b.WriteString(clipRaw(line, budget-len(prefix)))
			shownEnd = i
			partialLine = true
		}
		break
	}

	out := b.String()
	if shownEnd < start {
		return "read error: budget too small to return any content"
	}
	if !partialLine && shownEnd >= end {
		return out
	}
	hint := func(next int) string {
		if continueHint != nil {
			return continueHint(next)
		}
		return fmt.Sprintf("start_line=%d", next)
	}
	if partialLine {
		return out + fmt.Sprintf("\n[truncated: showed partial line %d of %d total. Continue with %s (line still incomplete)]",
			shownEnd, total, hint(shownEnd))
	}
	next := shownEnd + 1
	return out + fmt.Sprintf("\n[truncated: showed lines %d-%d of %d total. Continue with %s]",
		start, shownEnd, total, hint(next))
}

func (tc *ToolCtx) readWorkspaceFile(path string, startLine, endLine int) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	tc.usedCode = true
	full, err := tc.resolveWorkspace(path)
	if err != nil {
		return "read error: " + err.Error()
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "read error: " + err.Error()
	}
	return readFileWindow(string(b), startLine, endLine, readBudgetSingle, func(next int) string {
		if endLine > 0 {
			return fmt.Sprintf("read_file path=%q start_line=%d end_line=%d", path, next, endLine)
		}
		return fmt.Sprintf("read_file path=%q start_line=%d", path, next)
	})
}

// patchOp is one atomic edit inside apply_patch. Find must match exactly once
// in the in-memory buffer at the moment the op runs.
type patchOp struct {
	Op      string `json:"op"`      // replace | insert_after | insert_before | delete
	Find    string `json:"find"`    // exact text anchor (required)
	Replace string `json:"replace"` // replace op only
	Text    string `json:"text"`    // insert_after / insert_before
}

func (tc *ToolCtx) applyPatch(path string, ops []patchOp) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	tc.usedCode = true
	if len(ops) == 0 {
		return "patch error: ops must be a non-empty array"
	}
	full, err := tc.resolveWorkspace(path)
	if err != nil {
		return "patch error: " + err.Error()
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return "patch error: " + err.Error()
	}
	old := string(raw)
	next, err := applyOps(old, ops)
	if err != nil {
		return "patch error: " + err.Error()
	}
	if next == old {
		return fmt.Sprintf("patched %s: %d ops (+0 -0 lines) — content unchanged", path, len(ops))
	}
	if err := os.WriteFile(full, []byte(next), 0o644); err != nil {
		return "patch error: " + err.Error()
	}
	add, del := lineDiffCounts(old, next)
	return fmt.Sprintf("patched %s: %d ops (+%d -%d lines)", path, len(ops), add, del)
}

// applyOps applies every op to an in-memory copy. On any failure the original
// is untouched by the caller (we never write partial results).
func applyOps(content string, ops []patchOp) (string, error) {
	for i, op := range ops {
		op.Op = strings.ToLower(strings.TrimSpace(op.Op))
		if op.Find == "" {
			return "", fmt.Errorf("op %d (%s): find must be non-empty", i+1, op.Op)
		}
		n := strings.Count(content, op.Find)
		if n != 1 {
			return "", fmt.Errorf("op %d (%s): find matched %d times (need exactly 1)", i+1, op.Op, n)
		}
		switch op.Op {
		case "replace":
			content = strings.Replace(content, op.Find, op.Replace, 1)
		case "delete":
			content = strings.Replace(content, op.Find, "", 1)
		case "insert_after":
			idx := strings.Index(content, op.Find)
			at := idx + len(op.Find)
			content = content[:at] + op.Text + content[at:]
		case "insert_before":
			idx := strings.Index(content, op.Find)
			content = content[:idx] + op.Text + content[idx:]
		default:
			return "", fmt.Errorf("op %d: unknown op %q (want replace|insert_after|insert_before|delete)", i+1, op.Op)
		}
	}
	return content, nil
}

// lineDiffCounts returns how many lines were added/removed between before and
// after (LCS-based). A modified line counts as +1 -1.
func lineDiffCounts(before, after string) (added, removed int) {
	if before == after {
		return 0, 0
	}
	a := splitDiffLines(before)
	b := splitDiffLines(after)
	lcs := lcsLength(a, b)
	return len(b) - lcs, len(a) - lcs
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	// A trailing newline is line-terminator, not an extra blank line.
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""} // content was a single "\n"
	}
	return strings.Split(s, "\n")
}

func lcsLength(a, b []string) int {
	na, nb := len(a), len(b)
	if na == 0 || nb == 0 {
		return 0
	}
	// Two-row DP keeps memory O(min(n,m)).
	if nb < na {
		a, b = b, a
		na, nb = nb, na
	}
	prev := make([]int, na+1)
	cur := make([]int, na+1)
	for j := 1; j <= nb; j++ {
		for i := 1; i <= na; i++ {
			if a[i-1] == b[j-1] {
				cur[i] = prev[i-1] + 1
			} else if prev[i] >= cur[i-1] {
				cur[i] = prev[i]
			} else {
				cur[i] = cur[i-1]
			}
		}
		prev, cur = cur, prev
		for i := range cur {
			cur[i] = 0
		}
	}
	return prev[na]
}

const maxSearchResults = 50
const maxSearchFileBytes = 1 << 20 // skip files larger than 1 MiB

var searchSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "tmp": true, "log": true,
}

func (tc *ToolCtx) searchCode(pattern, pathFilter, glob string) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	tc.usedCode = true
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "search error: pattern is required"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "search error: bad pattern: " + err.Error()
	}
	root := tc.cfg.Workspace
	pathFilter = strings.TrimSpace(pathFilter)
	if pathFilter != "" {
		full, err := tc.resolveWorkspace(pathFilter)
		if err != nil {
			return "search error: " + err.Error()
		}
		fi, err := os.Stat(full)
		if err != nil {
			return "search error: " + err.Error()
		}
		if !fi.IsDir() {
			return searchOneFile(pathFilter, full, re, glob)
		}
		root = full
	}
	if err := os.MkdirAll(tc.cfg.Workspace, 0o755); err != nil {
		return "search error: " + err.Error()
	}

	var b strings.Builder
	matches := 0
	truncated := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		name := d.Name()
		if d.IsDir() {
			if searchSkipDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		if truncated {
			return fs.SkipAll
		}
		rel, err := filepath.Rel(tc.cfg.Workspace, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if !searchGlobOK(relSlash, glob) {
			return nil
		}
		n, hitTrunc := appendSearchHits(&b, relSlash, p, re, maxSearchResults-matches)
		matches += n
		if hitTrunc || matches >= maxSearchResults {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "search error: " + err.Error()
	}
	return finishSearchOutput(&b, matches, truncated)
}

func searchOneFile(rel, full string, re *regexp.Regexp, glob string) string {
	relSlash := filepath.ToSlash(rel)
	if !searchGlobOK(relSlash, glob) {
		return "no matches"
	}
	var b strings.Builder
	n, trunc := appendSearchHits(&b, relSlash, full, re, maxSearchResults)
	return finishSearchOutput(&b, n, trunc)
}

// searchGlobOK reports whether relSlash matches an optional filepath.Match glob
// against the full relative path or the basename (same rules as local search).
func searchGlobOK(relSlash, glob string) bool {
	if glob == "" {
		return true
	}
	base := filepath.Base(relSlash)
	okPath, _ := filepath.Match(glob, relSlash)
	okBase, _ := filepath.Match(glob, base)
	return okPath || okBase
}

// searchPathPrefixOK reports whether slash-path is under optional prefix
// (exact file or directory prefix). Empty prefix matches everything.
func searchPathPrefixOK(path, prefix string) bool {
	prefix = strings.Trim(filepath.ToSlash(strings.TrimSpace(prefix)), "/")
	if prefix == "" {
		return true
	}
	path = strings.Trim(filepath.ToSlash(path), "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// searchSkipRemotePath is true when any path component is a noise directory.
func searchSkipRemotePath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if searchSkipDirs[part] {
			return true
		}
	}
	return false
}

func finishSearchOutput(b *strings.Builder, matches int, truncated bool) string {
	if matches == 0 {
		return "no matches"
	}
	out := strings.TrimRight(b.String(), "\n")
	if truncated {
		out += fmt.Sprintf("\n[%d results shown, truncated — narrow pattern or path]", matches)
	}
	return out
}

func appendSearchHits(b *strings.Builder, relSlash, full string, re *regexp.Regexp, budget int) (int, bool) {
	if budget <= 0 {
		return 0, true
	}
	fi, err := os.Stat(full)
	if err != nil || fi.Size() > maxSearchFileBytes {
		return 0, false
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return 0, false
	}
	return appendContentSearchHits(b, relSlash, raw, re, budget)
}

// appendContentSearchHits scans raw file bytes for re and appends path:line hits.
// Shared by local workspace search and remote github search_code.
func appendContentSearchHits(b *strings.Builder, relSlash string, raw []byte, re *regexp.Regexp, budget int) (int, bool) {
	if budget <= 0 {
		return 0, true
	}
	if len(raw) > maxSearchFileBytes || isBinaryContent(raw) {
		return 0, false
	}
	matches := 0
	data := raw
	lineNo := 1
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i]
			data = data[i+1:]
		} else {
			line = data
			data = nil
		}
		if re.Match(line) {
			text := string(line)
			if !utf8.ValidString(text) {
				text = strings.ToValidUTF8(text, "�")
			}
			fmt.Fprintf(b, "%s:%d: %s\n", relSlash, lineNo, text)
			matches++
			if matches >= budget {
				return matches, true
			}
		}
		lineNo++
	}
	return matches, false
}

func isBinaryContent(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	if bytes.IndexByte(data[:n], 0) >= 0 {
		return true
	}
	return false
}

// SetupGit configures git ONCE at startup so `git push` to https GitHub
// remotes authenticates as Vela's own account. Crucially it keeps ALL of the
// bot's git state in an ISOLATED config + credentials file it owns (under the
// data dir) and never touches the operator's ~/.gitconfig: it points
// GIT_CONFIG_GLOBAL at the bot's own file and exports that into this process,
// so every `git` the shell tool runs inherits it. No-op when GITHUB_TOKEN is
// unset. This matters when Vela runs as a normal user (a dev box / the
// Mini bench): without it, `git config --global` would hijack that user's
// identity and credentials.
func SetupGit(cfg *Config) {
	if cfg.GitHubToken == "" {
		return
	}
	dir := filepath.Join(cfg.DataDir, "git")
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs // absolute so git resolves it from any workspace cwd
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	cred := filepath.Join(dir, "credentials")
	line := "https://x-access-token:" + cfg.GitHubToken + "@github.com\n"
	if err := os.WriteFile(cred, []byte(line), 0o600); err != nil {
		return
	}
	gc := filepath.Join(dir, "config")
	// Export to THIS process so the shell tool's git subprocesses use it too.
	_ = os.Setenv("GIT_CONFIG_GLOBAL", gc)
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+gc)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = env
		_ = cmd.Run()
	}
	run("config", "--global", "credential.helper", "store --file="+cred)
	run("config", "--global", "--add", "safe.directory", "*")
	if cfg.GitName != "" {
		run("config", "--global", "user.name", cfg.GitName)
	}
	if cfg.GitEmail != "" {
		run("config", "--global", "user.email", cfg.GitEmail)
	}
}
