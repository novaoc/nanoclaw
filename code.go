package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Code capability: a shell + file tools so Vela can write code, install
// libraries, and push to her own GitHub. HIGH TRUST — a shell can read
// everything the process can (env, other users' encrypted wallet material),
// so it's gated to an explicit coder allowlist (NANOCLAW_CODERS). Empty
// allowlist = the whole capability is off.

func (tc *ToolCtx) isCoder() bool { return tc.cfg.Coders[tc.authorID] }

const coderOnly = "REFUSED: running code/shell is limited to the coder allowlist (NANOCLAW_CODERS). " +
	"Tell this user they're not authorized to run commands on the box."

const codeInterlock = "REFUSED: code/shell is disabled because this process also holds wallet keys " +
	"(NANOCLAW_SECRET). Custody and code execution must run in SEPARATE processes — a shell here could " +
	"read the secret and every user's keys. Move key custody to clawvault first. Tell the user this plainly."

// codeGate is the single authorization point for all code tools: the
// custody/execution interlock first, then the coder allowlist.
func (tc *ToolCtx) codeGate() string {
	if tc.cfg.CodeInterlockTripped() {
		return codeInterlock
	}
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
	full, err := tc.resolveWorkspace(path)
	if err != nil {
		return "write error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "write error: " + err.Error()
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "write error: " + err.Error()
	}
	return fmt.Sprintf("wrote %s (%d bytes)", path, len(content))
}

func (tc *ToolCtx) readWorkspaceFile(path string) string {
	if g := tc.codeGate(); g != "" {
		return g
	}
	full, err := tc.resolveWorkspace(path)
	if err != nil {
		return "read error: " + err.Error()
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "read error: " + err.Error()
	}
	return clip(string(b), 6000)
}

// SetupGit configures git ONCE at startup so `git push` to https GitHub
// remotes authenticates as Vela's own account. The token lives only in the
// bot's own credential file (root-600) and env — same trust as its other
// secrets. No-op when GITHUB_TOKEN is unset.
func SetupGit(cfg *Config) {
	if cfg.GitHubToken == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cred := filepath.Join(home, ".git-credentials")
	line := "https://x-access-token:" + cfg.GitHubToken + "@github.com\n"
	if err := os.WriteFile(cred, []byte(line), 0o600); err != nil {
		return
	}
	run := func(args ...string) { _ = exec.Command("git", args...).Run() }
	run("config", "--global", "credential.helper", "store")
	run("config", "--global", "--add", "safe.directory", "*")
	if cfg.GitName != "" {
		run("config", "--global", "user.name", cfg.GitName)
	}
	if cfg.GitEmail != "" {
		run("config", "--global", "user.email", cfg.GitEmail)
	}
}
