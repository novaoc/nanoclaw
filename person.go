package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Person impressions, flat-file sized: Vela keeps a small evolving note about each person she's seen talk: a paragraph
// impression, a one-line short form (injected into her prompt when answering
// that person), and the nickname she privately calls them. Messages accrue
// per person; once enough pile up (impressionAfter), one cheap LLM call
// rewrites the impression from the old one + what they've said since. So
// familiarity is earned, drifts with evidence, and survives reboots as one
// JSON file per person under data/persons/.

const (
	impressionAfter = 45 // messages between impression rewrites
	pendingCap      = 60 // keep at most this many unprocessed messages
	personSaveEvery = 10 // flush to SD every Nth message (flash wear)
)

type Person struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"` // latest platform username
	Nick       string    `json:"nick"` // what Vela privately calls them
	Impression string    `json:"impression"`
	Short      string    `json:"short"` // ≤ ~140 chars, goes in the prompt
	Seen       int       `json:"seen"`  // total messages observed
	Pending    []string  `json:"pending"`
	Updated    time.Time `json:"updated"`
}

type People struct {
	mu       sync.Mutex
	cfg      *Config
	llm      *LLM
	byID     map[string]*Person
	updating map[string]bool
}

func NewPeople(cfg *Config, llm *LLM) *People {
	return &People{cfg: cfg, llm: llm, byID: map[string]*Person{}, updating: map[string]bool{}}
}

func (p *People) path(id string) string {
	return filepath.Join(p.cfg.DataDir, "persons", unsafeName.ReplaceAllString(id, "-")+".json")
}

func (p *People) load(id, name string) *Person {
	if per, ok := p.byID[id]; ok {
		if name != "" {
			per.Name = name
		}
		return per
	}
	per := &Person{ID: id, Name: name}
	if b, err := os.ReadFile(p.path(id)); err == nil {
		_ = json.Unmarshal(b, per)
		if name != "" {
			per.Name = name
		}
	}
	p.byID[id] = per
	return per
}

func (p *People) save(per *Person) {
	b, err := json.Marshal(per)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.path(per.ID)), 0o755)
	_ = os.WriteFile(p.path(per.ID), b, 0o600)
}

// Note records one observed message and, at the threshold, kicks off an async
// impression rewrite. Never blocks the message path.
func (p *People) Note(id, name, content string) {
	if id == "" || strings.TrimSpace(content) == "" {
		return
	}
	p.mu.Lock()
	per := p.load(id, name)
	per.Seen++
	per.Pending = append(per.Pending, clip(content, 200))
	if len(per.Pending) > pendingCap {
		per.Pending = per.Pending[len(per.Pending)-pendingCap:]
	}
	due := len(per.Pending) >= impressionAfter && !p.updating[id]
	if due {
		p.updating[id] = true
	} else if per.Seen%personSaveEvery == 0 {
		p.save(per)
	}
	p.mu.Unlock()
	if due {
		go p.refresh(id)
	}
}

// Short returns the prompt-ready one-liner for a person ("" until known).
func (p *People) Short(id string) string {
	if id == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	per := p.load(id, "")
	if per.Short == "" {
		return ""
	}
	nick := ""
	if per.Nick != "" {
		nick = " (you privately think of them as " + per.Nick + ")"
	}
	return fmt.Sprintf("%s%s: %s", per.Name, nick, per.Short)
}

// refresh rewrites one person's impression from the old one plus what they've
// said since — subjective, concrete, Vela's own view. Failure just leaves the
// old impression standing (pending stays, retried at the next threshold).
func (p *People) refresh(id string) {
	p.mu.Lock()
	per := p.load(id, "")
	name, old, nick := per.Name, per.Impression, per.Nick
	msgs := append([]string(nil), per.Pending...)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(`You are Vela, updating your private read on someone in the Discord server you live in.

Person: %s
Your current impression (may be empty): %s
Your private nickname for them (may be empty): %s

What they've said lately (a sample, oldest first):
%s

Rewrite your impression: 2-4 plain sentences, subjective and concrete — what they care about, how they talk, how you get along. Evolve the old impression, don't restart it unless the evidence contradicts it. Also produce a short form (max 140 chars) you can keep in mind mid-conversation, and a nickname you privately call them (keep the current one unless a better one is obvious).

Reply with ONLY this JSON: {"impression":"...","short":"...","nick":"..."}`,
		name, orNone(old), orNone(nick), strings.Join(msgs, "\n"))

	msg, err := p.llm.Chat(ctx, []Msg{{Role: "user", Content: prompt}}, nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer func() { delete(p.updating, id) }()
	per = p.load(id, "")
	if err != nil {
		log.Printf("impression update %s: %v", id, err)
		return
	}
	var out struct{ Impression, Short, Nick string }
	if jerr := json.Unmarshal([]byte(extractJSON(msg.Content)), &out); jerr != nil || strings.TrimSpace(out.Impression) == "" {
		log.Printf("impression update %s: bad response", id)
		return
	}
	per.Impression = strings.TrimSpace(out.Impression)
	per.Short = clip(strings.TrimSpace(out.Short), 160)
	if n := strings.TrimSpace(out.Nick); n != "" {
		per.Nick = clip(n, 40)
	}
	per.Pending = nil
	per.Updated = time.Now()
	p.save(per)
	log.Printf("impression updated for %s (%d msgs)", per.Name, len(msgs))
}

// extractJSON pulls the first {...} block out of a reply that may wrap it in
// prose or a code fence.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
