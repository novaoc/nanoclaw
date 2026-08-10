package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OwnedThreads remembers forum threads Vela created. Replies in those threads
// are addressed to her even without another @mention; otherwise a request post
// feels like a dead drop after she publishes its plan.
type OwnedThreads struct {
	mu   sync.RWMutex
	path string
	ids  map[string]time.Time
}

func NewOwnedThreads(cfg *Config) *OwnedThreads {
	o := &OwnedThreads{
		path: filepath.Join(cfg.DataDir, "owned-forum-threads.json"),
		ids:  map[string]time.Time{},
	}
	if b, err := os.ReadFile(o.path); err == nil {
		_ = json.Unmarshal(b, &o.ids)
	}
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	for id, created := range o.ids {
		if created.Before(cutoff) {
			delete(o.ids, id)
		}
	}
	return o
}

func (o *OwnedThreads) Add(id string) {
	if id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ids[id] = time.Now().UTC()
	if b, err := json.Marshal(o.ids); err == nil {
		_ = os.WriteFile(o.path, b, 0o600)
	}
}

func (o *OwnedThreads) Has(id string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, ok := o.ids[id]
	return ok
}
