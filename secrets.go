package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ephemeral secret store for deploy keys (Hetzner tokens, etc.). The design
// keeps secret VALUES out of every place a leak could happen — they never
// enter the LLM context, the channel history, MEMORY.md, or the logs. Secrets
// are added on the box (the /keys slash command was removed); they land here,
// on disk at 0600, and are exposed to the shell tool ONLY as environment
// variables, by NAME. The model uses "$HETZNER_TOKEN" without ever seeing the
// value, and wipes them with clear_secrets when the task is done. A TTL
// backstops a forgotten wipe.

type secretVal struct {
	Value string
	Added time.Time
}

type SecretStore struct {
	mu   sync.Mutex
	path string
	ttl  time.Duration
	m    map[string]secretVal
}

// secretName normalizes a submitted name to an env-var-safe key:
// "Hetzner API" -> "HETZNER_API". Empty if nothing usable remains.
var nonEnv = regexp.MustCompile(`[^A-Z0-9_]`)

func secretName(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	s = nonEnv.ReplaceAllString(s, "")
	s = strings.TrimLeft(s, "0123456789_")
	return s
}

func NewSecretStore(dataDir string, ttl time.Duration) *SecretStore {
	s := &SecretStore{
		path: filepath.Join(dataDir, "secrets.env"),
		ttl:  ttl,
		m:    map[string]secretVal{},
	}
	s.load()
	return s
}

// load reads persisted secrets (survive a reboot mid-deploy). Never logs values.
func (s *SecretStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			s.m[secretName(k)] = secretVal{Value: v, Added: time.Now()}
		}
	}
}

// persist writes the store at 0600. Best-effort; a write failure leaves the
// in-memory store authoritative for this run.
func (s *SecretStore) persist() {
	if len(s.m) == 0 {
		_ = os.Remove(s.path)
		return
	}
	var b strings.Builder
	for k, v := range s.m {
		fmt.Fprintf(&b, "%s=%s\n", k, v.Value)
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// expire drops secrets older than the TTL. Caller holds the lock.
func (s *SecretStore) expire() {
	if s.ttl <= 0 {
		return
	}
	now := time.Now()
	changed := false
	for k, v := range s.m {
		if now.Sub(v.Added) > s.ttl {
			delete(s.m, k)
			changed = true
		}
	}
	if changed {
		s.persist()
	}
}

func (s *SecretStore) Set(name, value string) (string, error) {
	n := secretName(name)
	if n == "" {
		return "", fmt.Errorf("bad key name — use letters/digits/underscore, e.g. HETZNER_TOKEN")
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("empty value")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[n] = secretVal{Value: value, Added: time.Now()}
	s.persist()
	return n, nil
}

// Names returns the secret names (never values), sorted, after TTL expiry.
func (s *SecretStore) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EnvPairs returns "NAME=value" pairs for injecting into the shell subprocess.
// This is the ONLY path a value leaves the store.
func (s *SecretStore) EnvPairs() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	out := make([]string, 0, len(s.m))
	for k, v := range s.m {
		out = append(out, k+"="+v.Value)
	}
	return out
}

func (s *SecretStore) Clear() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.m)
	s.m = map[string]secretVal{}
	_ = os.Remove(s.path)
	return n
}

func (s *SecretStore) Delete(name string) bool {
	n := secretName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[n]; !ok {
		return false
	}
	delete(s.m, n)
	s.persist()
	return true
}
