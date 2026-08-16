package main

// The worker's tool belt. Every path is jailed to one job's workspace, so a
// coding agent acting on untrusted spec text cannot read the DeepSeek key, the
// GitHub token, or another job's tree, and cannot escape onto the host. This
// jail is the reason a bad spec stays a failed build instead of a shell.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type workspace struct {
	root string // absolute, per-job
	repo string // owner/name
}

// resolve joins rel to the workspace root and refuses anything that escapes
// it — symlinks included, since the tree came from an untrusted clone.
func (ws *workspace) resolve(rel string) (string, error) {
	if strings.Contains(rel, "\x00") {
		return "", errors.New("path contains a null byte")
	}
	// Reject absolute paths outright rather than silently reinterpreting them
	// as workspace-relative (filepath.Join would quietly contain them, but a
	// path that looks absolute to the model should error, not surprise).
	if filepath.IsAbs(rel) {
		return "", errors.New("path must be relative to the workspace")
	}
	full := filepath.Join(ws.root, filepath.FromSlash(rel))
	clean := filepath.Clean(full)
	if clean != ws.root && !strings.HasPrefix(clean, ws.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	// A symlink anywhere along the resolved existing prefix could still point
	// out; reject if the real path of the deepest existing parent leaves root.
	dir := clean
	for dir != ws.root && dir != string(os.PathSeparator) {
		if _, err := os.Lstat(dir); err == nil {
			real, err := filepath.EvalSymlinks(dir)
			if err == nil && real != ws.root && !strings.HasPrefix(real, ws.root+string(os.PathSeparator)) {
				return "", fmt.Errorf("path %q resolves outside the workspace via a symlink", rel)
			}
			break
		}
		dir = filepath.Dir(dir)
	}
	return clean, nil
}

func (ws *workspace) readFile(rel string) (string, error) {
	full, err := ws.resolve(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if len(b) > 200_000 {
		return string(b[:200_000]) + "\n…[truncated]", nil
	}
	return string(b), nil
}

func (ws *workspace) writeFile(rel, content string) error {
	full, err := ws.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (ws *workspace) listTree(maxEntries int) (string, error) {
	var out []string
	err := filepath.WalkDir(ws.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == "node_modules" || name == "tmp") {
			return filepath.SkipDir
		}
		if p == ws.root {
			return nil
		}
		rel, _ := filepath.Rel(ws.root, p)
		out = append(out, filepath.ToSlash(rel))
		if len(out) >= maxEntries {
			return errors.New("__truncated__")
		}
		return nil
	})
	if err != nil && err.Error() != "__truncated__" {
		return "", err
	}
	return strings.Join(out, "\n"), nil
}

// runShell executes a command inside the workspace with a hard timeout and a
// stripped environment — the model gets a shell, but not the process env that
// holds the keys. Output is capped so a runaway command cannot blow memory.
func (ws *workspace) runShell(command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = ws.root
	// Minimal env: PATH and HOME only. No DEEPSEEK_API_KEY, no GITHUB_TOKEN,
	// no HOLODEX_TOKEN — none of the process secrets reach the sandbox.
	cmd.Env = []string{
		// sbin included so `service postgresql start` resolves; no process
		// secrets (no DEEPSEEK_API_KEY, GITHUB_TOKEN, or HOLODEX_TOKEN).
		"PATH=/usr/local/bundle/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + ws.root,
		"LANG=C.UTF-8",
		"RAILS_ENV=test",
		"BUNDLE_PATH=/usr/local/bundle",
		// Local unix-socket PostgreSQL, peer auth as root — matches the
		// foundation's own test setup, which connects over the socket with no
		// baked credentials.
		"DATABASE_URL=postgresql:///vela_worker_test",
		"SECRET_KEY_BASE=worker-local-test-secret",
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if len(out) > 40_000 {
		out = out[:20_000] + "\n…[middle truncated]…\n" + out[len(out)-20_000:]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("command timed out after %s", timeout)
	}
	return out, err
}

// runTests is the local foundation suite — the whole reason the worker exists.
// A red test here is caught in seconds instead of a 2–4 minute Holodex round
// trip. Runs the same steps Holodex's Dockerfile test target runs, minus the
// image build.
func (ws *workspace) runTests() (string, error) {
	// Ensure the local cluster is up and the socket role exists (idempotent),
	// then prepare the test DB and run the suite. Postgres startup noise is
	// dropped; only db:prepare + test output is returned.
	script := `
service postgresql start >/dev/null 2>&1 || true
su postgres -c "createuser --superuser root" >/dev/null 2>&1 || true
bin/rails db:prepare 2>&1 | tail -20
echo "=== test ==="
bin/rails test 2>&1
`
	return ws.runShell(script, 12*time.Minute)
}
