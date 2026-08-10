// holodeck — Vela's demo sandbox. A single static binary that hosts throwaway
// static sites: Vela POSTs an app's files to /api/deploy (bearer token) and
// gets back a live URL on its own subdomain (<slug>.demo.holode.xyz); a
// sweeper deletes every app 7 days after deploy. The GitHub repo is the
// permanent copy — the holodeck program always ends.
//
// It sits behind Caddy (TLS via on-demand certs; /api/tls-check is the "ask"
// endpoint that approves hostnames so strangers can't mint certs on the
// domain). Static files only — nothing uploaded here ever executes on the
// server. Per-app subdomains give each demo its own browser origin, so demos
// can't touch each other either.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxBody     = 12 << 20 // request cap; total file budget below
	maxFiles    = 64
	maxTotal    = 10 << 20
	sweepEvery  = time.Hour
	defaultTTL  = 7 * 24 * time.Hour
	defaultAddr = ":8700"
)

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

type server struct {
	data   string // apps live in <data>/apps/<slug>/
	token  string
	domain string // demo.holode.xyz — apps at <slug>.<domain>
	ttl    time.Duration
}

type deployReq struct {
	Name  string `json:"name"`
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

type meta struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

func main() {
	s := &server{
		data:   envOr("HOLODECK_DATA", "/srv/holodeck"),
		token:  os.Getenv("HOLODECK_TOKEN"),
		domain: envOr("HOLODECK_DOMAIN", "demo.holode.xyz"),
		ttl:    defaultTTL,
	}
	if s.token == "" {
		log.Fatal("HOLODECK_TOKEN is required")
	}
	if h := os.Getenv("HOLODECK_TTL_HOURS"); h != "" {
		if d, err := time.ParseDuration(h + "h"); err == nil && d > 0 {
			s.ttl = d
		}
	}
	if err := os.MkdirAll(filepath.Join(s.data, "apps"), 0o755); err != nil {
		log.Fatal(err)
	}
	go s.sweep()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/deploy", s.auth(s.handleDeploy))
	mux.HandleFunc("GET /api/apps", s.auth(s.handleList))
	mux.HandleFunc("DELETE /api/apps/{slug}", s.auth(s.handleDelete))
	mux.HandleFunc("GET /api/tls-check", s.handleTLSCheck) // caddy asks; no auth
	mux.HandleFunc("/", s.handleServe)

	addr := envOr("HOLODECK_ADDR", defaultAddr)
	log.Printf("holodeck up on %s — apps at *.%s, ttl %s", addr, s.domain, s.ttl)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// slugify makes the app's URL label: sanitized name + 4 random hex chars, so
// names can't collide with or squat on each other across deploys.
func slugify(name string) string {
	base := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	base = strings.Trim(base, "-")
	if len(base) > 30 {
		base = base[:30]
	}
	if base == "" {
		base = "app"
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return base + "-" + hex.EncodeToString(b)
}

// cleanPath validates one deployed file path: relative, no escapes, sane depth.
func cleanPath(p string) (string, bool) {
	p = filepath.ToSlash(filepath.Clean("/" + p))[1:] // forces inside root
	if p == "" || strings.HasPrefix(p, ".") || strings.Contains(p, "/.") || len(p) > 200 {
		return "", false
	}
	return p, true
}

func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 || len(req.Files) > maxFiles {
		http.Error(w, fmt.Sprintf("need 1-%d files", maxFiles), http.StatusBadRequest)
		return
	}
	hasIndex, total := false, 0
	for _, f := range req.Files {
		if p, ok := cleanPath(f.Path); !ok {
			http.Error(w, "bad path: "+f.Path, http.StatusBadRequest)
			return
		} else if p == "index.html" {
			hasIndex = true
		}
		total += len(f.Content)
	}
	if !hasIndex {
		http.Error(w, "an index.html at the root is required (it's the homepage)", http.StatusBadRequest)
		return
	}
	if total > maxTotal {
		http.Error(w, "app too large (10MB cap)", http.StatusBadRequest)
		return
	}
	slug := slugify(req.Name)
	dir := filepath.Join(s.data, "apps", slug)
	for _, f := range req.Files {
		p, _ := cleanPath(f.Path)
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	m := meta{Name: req.Name, Created: time.Now().UTC()}
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(filepath.Join(dir, ".meta.json"), b, 0o644)
	}
	log.Printf("deployed %s (%d files, %dKB)", slug, len(req.Files), total/1024)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":     fmt.Sprintf("https://%s.%s/", slug, s.domain),
		"slug":    slug,
		"expires": m.Created.Add(s.ttl).Format(time.RFC3339),
	})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(filepath.Join(s.data, "apps"))
	out := []map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := s.readMeta(e.Name())
		out = append(out, map[string]string{
			"slug": e.Name(), "name": m.Name,
			"created": m.Created.Format(time.RFC3339),
			"expires": m.Created.Add(s.ttl).Format(time.RFC3339),
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slugRe.MatchString(slug) || slug == "" || strings.Contains(slug, "/") {
		http.Error(w, "bad slug", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.data, "apps", slug)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("deleted %s", slug)
	w.WriteHeader(http.StatusNoContent)
}

// handleTLSCheck is Caddy's on_demand_tls "ask": approve certificates only for
// the base domain and subdomains of apps that actually exist — a stranger
// pointing DNS here can't mint certs for arbitrary names.
func (s *server) handleTLSCheck(w http.ResponseWriter, r *http.Request) {
	d := strings.ToLower(r.URL.Query().Get("domain"))
	if d == s.domain {
		return // 200
	}
	if slug, ok := strings.CutSuffix(d, "."+s.domain); ok && !strings.Contains(slug, ".") {
		if st, err := os.Stat(filepath.Join(s.data, "apps", slug)); err == nil && st.IsDir() {
			return // 200
		}
	}
	http.Error(w, "unknown host", http.StatusForbidden)
}

// handleServe routes by Host: <slug>.<domain> serves that app's files;
// the bare domain gets a landing page. Directory listings are disabled and
// dotfiles (including .meta.json) are never served.
func (s *server) handleServe(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.Split(r.Host, ":")[0])
	w.Header().Set("X-Robots-Tag", "noindex")
	if host == s.domain || host != "" && !strings.HasSuffix(host, "."+s.domain) {
		s.landing(w, r)
		return
	}
	slug := strings.TrimSuffix(host, "."+s.domain)
	dir := filepath.Join(s.data, "apps", slug)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		http.Error(w, "this holodeck program has ended (demos expire after 7 days)", http.StatusNotFound)
		return
	}
	p, ok := cleanPath(strings.TrimPrefix(r.URL.Path, "/"))
	if !ok && strings.TrimPrefix(r.URL.Path, "/") != "" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(dir, filepath.FromSlash(p))
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		full = filepath.Join(full, "index.html")
		if _, err := os.Stat(full); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	http.ServeFile(w, r, full)
}

func (s *server) landing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>holodeck</title>
<style>body{font-family:system-ui;background:#0A0D13;color:#F5F6F8;display:grid;place-items:center;min-height:100vh;margin:0}main{text-align:center;line-height:1.7}small{color:#8b93a3}</style>
<main><h1>holodeck</h1><p>Vela's demo deck — ask her to build something and put it online.</p>
<small>every program here self-destructs %d days after deploy · the repo is the keepsake</small></main>`, int(s.ttl.Hours()/24))
}

func (s *server) readMeta(slug string) meta {
	var m meta
	if b, err := os.ReadFile(filepath.Join(s.data, "apps", slug, ".meta.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m.Created.IsZero() {
		if st, err := os.Stat(filepath.Join(s.data, "apps", slug)); err == nil {
			m.Created = st.ModTime()
		}
	}
	return m
}

// sweep deletes apps past their TTL, hourly. The repo on GitHub is the
// permanent copy; the deck always resets.
func (s *server) sweep() {
	for {
		entries, _ := os.ReadDir(filepath.Join(s.data, "apps"))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if m := s.readMeta(e.Name()); time.Since(m.Created) > s.ttl {
				if err := os.RemoveAll(filepath.Join(s.data, "apps", e.Name())); err == nil {
					log.Printf("expired %s (deployed %s)", e.Name(), m.Created.Format("2006-01-02"))
				}
			}
		}
		time.Sleep(sweepEvery)
	}
}
