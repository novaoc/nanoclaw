package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Expression learning: Vela periodically reads a sample of a channel's chatter and distills
// HOW that room talks into small reusable rules — "when [teasing someone],
// say it like [short lowercase jab, no punctuation]". Rules live in one JSON
// file per channel, carry a use count, get reinforced when re-learned and
// decay when ignored, and a weighted sample of them rides into the system
// prompt — so each channel slowly gets its own Vela. She learns the room's
// voice, not its facts (facts are MEMORY.md's job).

const (
	exprPerChannel  = 100           // cap; weighted-random eviction beyond it
	exprLearnEvery  = 6 * time.Hour // per-channel cooldown between passes
	exprSampleSize  = 25            // messages read per pass
	exprMinMessages = 25            // don't learn from a near-empty ring
	exprPromptCount = 6             // rules injected per turn
	exprUseBump     = 0.006         // tiny reinforcement when actually used
	exprCountCap    = 5.0
)

type Expression struct {
	Situation string    `json:"situation"`
	Style     string    `json:"style"`
	Count     float64   `json:"count"`
	Last      time.Time `json:"last"`
}

type Expressions struct {
	mu        sync.Mutex
	cfg       *Config
	llm       *LLM
	social    *Social
	byCh      map[string][]*Expression
	lastLearn map[string]time.Time
}

func NewExpressions(cfg *Config, llm *LLM, social *Social) *Expressions {
	return &Expressions{cfg: cfg, llm: llm, social: social,
		byCh: map[string][]*Expression{}, lastLearn: map[string]time.Time{}}
}

func (e *Expressions) path(ch string) string {
	return filepath.Join(e.cfg.DataDir, "expressions", unsafeName.ReplaceAllString(ch, "-")+".json")
}

func (e *Expressions) load(ch string) []*Expression {
	if x, ok := e.byCh[ch]; ok {
		return x
	}
	var x []*Expression
	if b, err := os.ReadFile(e.path(ch)); err == nil {
		_ = json.Unmarshal(b, &x)
	}
	e.byCh[ch] = x
	return x
}

func (e *Expressions) save(ch string) {
	b, err := json.Marshal(e.byCh[ch])
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(e.path(ch)), 0o755)
	_ = os.WriteFile(e.path(ch), b, 0o600)
}

// PromptBlock returns up to exprPromptCount learned rules for the system
// prompt, sampled weighted by count (well-worn habits surface more), and
// gives the chosen ones a tiny use-bump — use it or lose it.
func (e *Expressions) PromptBlock(ch string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	all := e.load(ch)
	if len(all) == 0 {
		return ""
	}
	picked := weightedSample(all, exprPromptCount)
	var b strings.Builder
	b.WriteString("How this channel talks — habits you've picked up here; use them when they fit, never force them:\n")
	for _, x := range picked {
		fmt.Fprintf(&b, "- when %s, %s\n", x.Situation, x.Style)
		x.Count = math.Min(x.Count+exprUseBump, exprCountCap)
	}
	return b.String()
}

// Learn runs one learning pass over a channel's recent chatter.
func (e *Expressions) Learn(ch string) {
	msgs := e.social.Sample(ch, exprSampleSize)
	if len(msgs) < exprMinMessages {
		return
	}
	var lines []string
	for _, m := range msgs {
		lines = append(lines, m.Author+": "+m.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	prompt := `Below is a sample of chatter from one Discord channel. Extract up to 5 reusable STYLE rules that capture how this room talks — tone, word choice, sentence shape, in-jokes. NOT facts, NOT topics: voice only. Skip slurs and anything hateful. Each rule: a situation and how to phrase it.

Reply with ONLY a JSON array like:
[{"situation":"reacting to good news","style":"short lowercase hype, no punctuation"},...]

Chatter:
` + strings.Join(lines, "\n")
	msg, err := e.llm.Chat(ctx, []Msg{{Role: "user", Content: prompt}}, nil)
	if err != nil {
		log.Printf("expression learn %s: %v", ch, err)
		return
	}
	var learned []Expression
	if json.Unmarshal([]byte(extractJSONArray(msg.Content)), &learned) != nil {
		log.Printf("expression learn %s: bad response", ch)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	all := e.load(ch)
	all = decayExpressions(all)
	added := 0
	for i := range learned {
		l := &learned[i]
		l.Situation, l.Style = strings.TrimSpace(l.Situation), strings.TrimSpace(l.Style)
		if l.Situation == "" || l.Style == "" {
			continue
		}
		if x := findSimilar(all, l.Style); x != nil {
			x.Count++ // reinforcement, not duplication
			x.Last = time.Now()
			continue
		}
		all = append(all, &Expression{Situation: l.Situation, Style: clip(l.Style, 200), Count: 1, Last: time.Now()})
		added++
	}
	for len(all) > exprPerChannel {
		all = evictOne(all)
	}
	e.byCh[ch] = all
	e.save(ch)
	e.lastLearn[ch] = time.Now()
	log.Printf("expressions %s: +%d new, %d total", ch, added, len(all))
}

// Run is the background learner: every 30 minutes, learn from any channel
// with enough fresh chatter whose cooldown has passed. Started once at boot;
// exits when ctx does.
func (e *Expressions) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, ch := range e.social.Channels(exprMinMessages) {
				e.mu.Lock()
				due := time.Since(e.lastLearn[ch]) > exprLearnEvery
				e.mu.Unlock()
				if due {
					e.Learn(ch)
				}
			}
		}
	}
}

// decayExpressions applies quadratic time decay and drops the dead.
func decayExpressions(all []*Expression) []*Expression {
	out := all[:0]
	for _, x := range all {
		days := time.Since(x.Last).Hours() / 24
		if days > 30 {
			days = 30
		}
		x.Count -= 0.01 * days * days
		if x.Count > 0.01 {
			out = append(out, x)
		}
	}
	return out
}

// findSimilar treats two styles as the same habit when their word sets mostly
// overlap (the flat-file stand-in for embedding similarity).
func findSimilar(all []*Expression, style string) *Expression {
	a := wordSet(style)
	for _, x := range all {
		b := wordSet(x.Style)
		inter := 0
		for w := range a {
			if b[w] {
				inter++
			}
		}
		smaller := len(a)
		if len(b) < smaller {
			smaller = len(b)
		}
		if smaller > 0 && float64(inter)/float64(smaller) > 0.7 {
			return x
		}
	}
	return nil
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		out[w] = true
	}
	return out
}

// evictOne removes one expression, weighted toward rarely-used ones.
func evictOne(all []*Expression) []*Expression {
	weights := make([]float64, len(all))
	total := 0.0
	for i, x := range all {
		weights[i] = 1 / (x.Count + 0.1)
		total += weights[i]
	}
	r := rand.Float64() * total
	for i, w := range weights {
		if r -= w; r <= 0 {
			return append(all[:i], all[i+1:]...)
		}
	}
	return all[:len(all)-1]
}

// weightedSample picks up to n expressions weighted by count, no repeats.
func weightedSample(all []*Expression, n int) []*Expression {
	pool := append([]*Expression(nil), all...)
	var out []*Expression
	for len(out) < n && len(pool) > 0 {
		total := 0.0
		for _, x := range pool {
			total += x.Count + 0.1
		}
		r := rand.Float64() * total
		for i, x := range pool {
			if r -= x.Count + 0.1; r <= 0 {
				out = append(out, x)
				pool = append(pool[:i], pool[i+1:]...)
				break
			}
		}
	}
	return out
}

// extractJSONArray pulls the first [...] block out of a possibly-wrapped reply.
func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
