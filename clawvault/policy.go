package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Policy enforced INSIDE the vault, fail-closed: a per-user daily cap on
// fund-moving actions, a short cooldown between them, and an append-only
// audit log. Dollar-precise caps need parsing Bankr's response amounts (a
// later addition); until then the write-count cap + cooldown + audit are the
// backstop when Bankr's own limits aren't granular enough.

type Policy struct {
	mu          sync.Mutex
	dailyWrites map[string][]time.Time // uid → timestamps of writes today
	maxPerDay   int
	cooldown    time.Duration
	audit       *os.File
}

func NewPolicy(auditPath string, maxPerDay int, cooldown time.Duration) (*Policy, error) {
	f, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Policy{
		dailyWrites: map[string][]time.Time{},
		maxPerDay:   maxPerDay,
		cooldown:    cooldown,
		audit:       f,
	}, nil
}

// CheckWrite is called before a fund-moving action executes. Fail-closed:
// returns an error the caller must surface and NOT execute past.
func (p *Policy) CheckWrite(uid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// keep only the last 24h
	kept := p.dailyWrites[uid][:0]
	for _, t := range p.dailyWrites[uid] {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	p.dailyWrites[uid] = kept
	if len(kept) >= p.maxPerDay {
		return errors.New("daily transaction cap reached — try again tomorrow")
	}
	if len(kept) > 0 && now.Sub(kept[len(kept)-1]) < p.cooldown {
		return errors.New("too soon after your last transaction — wait a moment")
	}
	return nil
}

// RecordWrite marks a write as executed (counts toward the cap).
func (p *Policy) RecordWrite(uid string) {
	p.mu.Lock()
	p.dailyWrites[uid] = append(p.dailyWrites[uid], time.Now())
	p.mu.Unlock()
}

func (p *Policy) Audit(event, uid, detail string) {
	if p.audit == nil {
		return
	}
	rec, _ := json.Marshal(map[string]string{
		"ts": time.Now().UTC().Format(time.RFC3339), "event": event, "uid": uid, "detail": detail,
	})
	p.mu.Lock()
	p.audit.Write(append(rec, '\n'))
	p.mu.Unlock()
}

// ── pending confirmations (owned by the vault) ──────────────────────────

const confirmTTL = 5 * time.Minute

var (
	errExpired = errors.New("this confirmation expired or was already used")
	errNotYours = errors.New("only the person who requested this can confirm it")
)

type Pending struct {
	Token, UID, Channel, Prompt string
	created                     time.Time
}

type Confirmations struct {
	mu sync.Mutex
	m  map[string]*Pending
}

func NewConfirmations() *Confirmations { return &Confirmations{m: map[string]*Pending{}} }

func randToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Confirmations) Add(uid, channel, prompt string) *Pending {
	p := &Pending{Token: randToken(), UID: uid, Channel: channel, Prompt: prompt, created: time.Now()}
	c.mu.Lock()
	c.m[p.Token] = p
	for tok, x := range c.m {
		if time.Since(x.created) > confirmTTL {
			delete(c.m, tok)
		}
	}
	c.mu.Unlock()
	return p
}

func (c *Confirmations) Take(token, uid string) (*Pending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.m[token]
	if p == nil {
		return nil, errExpired
	}
	if p.UID != uid {
		return nil, errNotYours
	}
	delete(c.m, token)
	if time.Since(p.created) > confirmTTL {
		return nil, errExpired
	}
	return p, nil
}

func (c *Confirmations) Cancel(token, uid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.m[token]
	if p == nil {
		return errExpired
	}
	if p.UID != uid {
		return errNotYours
	}
	delete(c.m, token)
	return nil
}
