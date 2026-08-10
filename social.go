package main

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

// Group presence — the MaiBot lesson (0.6–0.8 era willingness model): a bot
// feels alive in a group not because of what it says but because of WHEN it
// chooses to say nothing. Vela sees every message in a guild channel, scores
// how interesting it is (all local math — no API call for skipped messages),
// and accumulates per-channel "willingness". Only when willingness clears the
// bar does a real agent turn run — and silence is the default outcome of
// every failure path. Mentions, DMs, and focus channels bypass all of this
// (they always answer, as before).

const (
	willCap      = 3.0
	willHalfLife = 60 * time.Second // idle channels drift back to silence
	replyCost    = 1.8              // spent on speaking — prevents machine-gunning
	backoffMin   = 15 * time.Second
	backoffMax   = 5 * time.Minute
	recentCap    = 80 // ring of recent chatter per channel (context + learning)
)

type ambientMsg struct {
	AuthorID string
	Author   string
	Content  string
	T        time.Time
}

type chanState struct {
	will     float64
	lastSeen time.Time
	backoff  time.Duration
	nextEval time.Time
	recent   []ambientMsg
}

type Social struct {
	mu   sync.Mutex
	cfg  *Config
	byCh map[string]*chanState
	// memory keyword cache — interest scoring greps MEMORY.md per message;
	// re-reading the file every time would grind the SD.
	memWords  map[string]bool
	memLoaded time.Time
	randFloat func() float64 // injectable for tests
}

func NewSocial(cfg *Config) *Social {
	return &Social{cfg: cfg, byCh: map[string]*chanState{}, randFloat: rand.Float64}
}

func (s *Social) state(ch string) *chanState {
	st, ok := s.byCh[ch]
	if !ok {
		st = &chanState{lastSeen: time.Now()}
		s.byCh[ch] = st
	}
	return st
}

// Observe records a guild message and — when decide is set — decides whether
// Vela chimes in unprompted. Messages she'll answer anyway (mentions, focus
// channels) are recorded with decide=false so the transcript and interest
// state stay complete without a wasted coin flip. Pure local math — the API
// is only touched if this returns true.
func (s *Social) Observe(ch, authorID, author, content string, decide bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(ch)
	now := time.Now()
	st.recent = append(st.recent, ambientMsg{AuthorID: authorID, Author: author, Content: clip(content, 300), T: now})
	if len(st.recent) > recentCap {
		st.recent = st.recent[len(st.recent)-recentCap:]
	}
	// lazy exponential decay since the last message
	if dt := now.Sub(st.lastSeen); dt > 0 {
		st.will *= math.Exp2(-dt.Seconds() / willHalfLife.Seconds())
	}
	st.lastSeen = now

	if interest := s.interest(content); interest > 0.3 {
		st.will = math.Min(st.will+interest-0.3, willCap)
	}
	if !decide || s.cfg.TalkValue <= 0 {
		return false // recorded for context/learning; no chime-in decision here
	}
	// Exponential no-action backoff (MaiBot 1.x reply_timing): once she's
	// decided to stay quiet, don't even reconsider for a while.
	if now.Before(st.nextEval) {
		return false
	}
	p := math.Min(math.Max(st.will-0.5, 0.01)*2, 1) * (s.cfg.TalkValue * 2)
	if s.randFloat() < p {
		st.will = math.Max(st.will-replyCost, 0)
		st.backoff, st.nextEval = 0, now
		return true
	}
	st.backoff = min(max(backoffMin, st.backoff*2), backoffMax)
	st.nextEval = now.Add(st.backoff)
	return false
}

// interest scores a message locally: a small length term (log-scaled), plus a
// boost when it touches topics Vela has long-term memories about — the
// memory-activation idea from MaiBot's hippocampus, done with token overlap
// instead of a graph.
func (s *Social) interest(content string) float64 {
	v := 0.01 + 0.04*math.Log10(float64(len(content))+1)/3
	words := s.memoryWords()
	seen := 0
	for _, w := range strings.Fields(strings.ToLower(content)) {
		w = strings.Trim(w, ".,!?;:\"'()[]")
		if len(w) >= 5 && words[w] {
			if seen++; seen >= 4 {
				break
			}
		}
	}
	return v + 0.15*float64(seen)
}

// memoryWords caches MEMORY.md's vocabulary for 5 minutes.
func (s *Social) memoryWords() map[string]bool {
	if s.memWords != nil && time.Since(s.memLoaded) < 5*time.Minute {
		return s.memWords
	}
	words := map[string]bool{}
	if b, err := os.ReadFile(memoryPath(s.cfg)); err == nil {
		for _, w := range strings.Fields(strings.ToLower(string(b))) {
			w = strings.Trim(w, ".,!?;:\"'()[]-*")
			if len(w) >= 5 {
				words[w] = true
			}
		}
	}
	s.memWords, s.memLoaded = words, time.Now()
	return words
}

// Recent renders the last n messages of channel chatter (newest last) so an
// ambient turn sees the conversation it's joining — the per-channel history
// only holds turns Vela was part of.
func (s *Social) Recent(ch string, n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byCh[ch]
	if !ok || len(st.recent) == 0 {
		return ""
	}
	msgs := st.recent
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Author, m.Content)
	}
	return b.String()
}

// Sample returns up to n recent messages for the expression learner and
// resets nothing — the learner tracks its own cursor via counts.
func (s *Social) Sample(ch string, n int) []ambientMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byCh[ch]
	if !ok {
		return nil
	}
	msgs := append([]ambientMsg(nil), st.recent...)
	if len(msgs) <= n {
		return msgs
	}
	rand.Shuffle(len(msgs), func(i, j int) { msgs[i], msgs[j] = msgs[j], msgs[i] })
	return msgs[:n]
}

// Channels lists channels with at least n recorded messages (learner targets).
func (s *Social) Channels(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for ch, st := range s.byCh {
		if len(st.recent) >= n {
			out = append(out, ch)
		}
	}
	return out
}
