package main

// The worker's eyes. A coding model writes SVG and CSS blind — it produced a
// brass capsule while calling it an hourglass, twice, because nothing ever
// showed it what its coordinates render as. This file boots the app it is
// building, screenshots real pages headless, and lets a vision model critique
// the result against the spec. Sight turns "confident wrong geometry" into a
// revision loop.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	previewPort  = 4321
	previewBoot  = 120 * time.Second
	shotViewport = "390,900" // phone-first, same bias as the products
)

// ensurePreview boots the app's dev server once per job (idempotent) and
// waits for it to answer. Dev environment: no production secrets, permissive
// host checking, its own database.
func (s *server) ensurePreview(id string, ws *workspace) error {
	s.store.mu.Lock()
	if s.previews == nil {
		s.previews = map[string]*exec.Cmd{}
	}
	if _, running := s.previews[id]; running {
		s.store.mu.Unlock()
		return nil
	}
	s.store.mu.Unlock()

	// Dev database + built assets, then boot.
	prep := `
service postgresql start >/dev/null 2>&1 || true
su postgres -c "createuser --superuser root" >/dev/null 2>&1 || true
DATABASE_URL=postgresql:///vela_worker_dev RAILS_ENV=development bin/rails db:prepare 2>&1 | tail -3
DATABASE_URL=postgresql:///vela_worker_dev RAILS_ENV=development bin/rails tailwindcss:build 2>&1 | tail -2
`
	if out, err := ws.runShell(prep, 5*time.Minute); err != nil {
		return fmt.Errorf("preview prep failed: %s", tailOf(out, 1200))
	}

	cmd := exec.Command("bin/rails", "server", "-e", "development",
		"-p", fmt.Sprintf("%d", previewPort), "-b", "127.0.0.1", "--no-log-to-stdout")
	cmd.Dir = ws.root
	cmd.Env = []string{
		"PATH=/usr/local/bundle/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + ws.root,
		"LANG=C.UTF-8",
		"RAILS_ENV=development",
		"BUNDLE_PATH=/usr/local/bundle",
		"DATABASE_URL=postgresql:///vela_worker_dev",
	}
	logf, _ := os.Create(filepath.Join(ws.root, "tmp", "preview.log"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // killable as a group
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("preview boot: %w", err)
	}
	s.store.mu.Lock()
	s.previews[id] = cmd
	s.store.mu.Unlock()

	deadline := time.Now().Add(previewBoot)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", previewPort)); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
	}
	tail, _ := ws.runShell("tail -30 tmp/preview.log", 10*time.Second)
	return fmt.Errorf("preview server did not come up in %s. Server log tail:\n%s", previewBoot, tail)
}

// stopPreview tears down the job's dev server (process group, so Puma's
// workers die with it).
func (s *server) stopPreview(id string) {
	s.store.mu.Lock()
	cmd := s.previews[id]
	delete(s.previews, id)
	s.store.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
}

// screenshotPage renders one path headless and returns the PNG bytes.
// virtual-time lets fonts, CSS animations, and first paints settle.
func (s *server) screenshotPage(ws *workspace, path string) ([]byte, error) {
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	out := filepath.Join(ws.root, "tmp", "shot.png")
	cmd := exec.Command("chromium",
		"--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars",
		"--window-size="+shotViewport,
		"--virtual-time-budget=5000",
		"--screenshot="+out,
		fmt.Sprintf("http://127.0.0.1:%d%s", previewPort, path),
	)
	cmd.Dir = ws.root
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + ws.root}
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("chromium: %v: %.400s", err, b)
	}
	png, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if len(png) < 2048 {
		return nil, fmt.Errorf("screenshot suspiciously small (%d bytes) — page may not have rendered", len(png))
	}
	return png, nil
}
