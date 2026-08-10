package main

import (
	"strings"
	"testing"
	"time"
)

func TestWillingnessGate(t *testing.T) {
	cfg := testCfg(t)
	cfg.TalkValue = 0.5
	s := NewSocial(cfg)
	s.randFloat = func() float64 { return 0.99 } // never fire on chance alone

	long := strings.Repeat("interesting words about agents and evals ", 5)
	if s.Observe("ch", "u1", "alice", long, true) {
		t.Fatal("one message should not clear the bar")
	}
	// force high willingness: probability must hit 1 and fire even at rand=0.99
	s.byCh["ch"].will = 3.0
	s.byCh["ch"].nextEval = time.Time{}
	if !s.Observe("ch", "u1", "alice", long, true) {
		t.Fatal("saturated willingness must fire")
	}
	// speaking spends willingness
	if w := s.byCh["ch"].will; w > 1.5 {
		t.Fatalf("reply cost not applied: %f", w)
	}
}

func TestWillingnessBackoffAndOff(t *testing.T) {
	cfg := testCfg(t)
	cfg.TalkValue = 0.5
	s := NewSocial(cfg)
	s.randFloat = func() float64 { return 0.99 }
	s.Observe("ch", "u1", "alice", "hello there friend", true)
	if s.byCh["ch"].backoff < backoffMin {
		t.Fatal("no-reply should start the backoff clock")
	}
	// decide=false records but never decides (and applies no backoff change)
	prev := s.byCh["ch"].backoff
	s.Observe("ch", "u1", "alice", "another message", false)
	if s.byCh["ch"].backoff != prev {
		t.Fatal("decide=false must not touch backoff")
	}
	// TalkValue 0 = ambient off, still records
	cfg.TalkValue = 0
	s2 := NewSocial(cfg)
	s2.randFloat = func() float64 { return 0 } // would always fire if allowed
	if s2.Observe("ch", "u1", "alice", "hello", true) {
		t.Fatal("TalkValue=0 must never chime in")
	}
	if len(s2.byCh["ch"].recent) != 1 {
		t.Fatal("message not recorded")
	}
}

func TestRecentTranscript(t *testing.T) {
	s := NewSocial(testCfg(t))
	s.Observe("ch", "u1", "alice", "first", false)
	s.Observe("ch", "u2", "bob", "second", false)
	got := s.Recent("ch", 10)
	if !strings.Contains(got, "alice: first") || !strings.Contains(got, "bob: second") {
		t.Fatalf("transcript wrong: %q", got)
	}
}

func TestHumanChunks(t *testing.T) {
	// several short sentences → 2-3 messages
	out := humanChunks("nice. that tracks. the eval numbers looked off anyway.")
	if len(out) < 2 || len(out) > 3 {
		t.Fatalf("expected 2-3 chunks, got %d: %v", len(out), out)
	}
	// one sentence → single message path
	if humanChunks("just one line here") != nil {
		t.Fatal("single sentence must not split")
	}
	// code stays whole
	if humanChunks("look:\n```go\nfmt.Println(1)\n```\nneat.") != nil {
		t.Fatal("code blocks must not split")
	}
	// links stay whole (previews break when a sentence is cut around them)
	if humanChunks("see https://x.ai for docs. it explains it. really.") != nil {
		t.Fatal("links must not split")
	}
	// long replies use the plain path
	if humanChunks(strings.Repeat("a solid sentence right here. ", 30)) != nil {
		t.Fatal("long text must not split")
	}
}

func TestIsPass(t *testing.T) {
	for _, s := range []string{"PASS", "pass", " PASS. ", `"PASS"`} {
		if !isPass(s) {
			t.Errorf("isPass(%q) = false", s)
		}
	}
	for _, s := range []string{"I'll pass on that one", "PASSING through", ""} {
		if isPass(s) {
			t.Errorf("isPass(%q) = true", s)
		}
	}
}

func TestExpressionMergeAndDecay(t *testing.T) {
	all := []*Expression{{Situation: "teasing", Style: "short lowercase jab no punctuation", Count: 2, Last: time.Now()}}
	if x := findSimilar(all, "short lowercase jab, no punctuation"); x == nil {
		t.Fatal("near-identical style should merge")
	}
	if x := findSimilar(all, "formal apology with full sentences"); x != nil {
		t.Fatal("different style must not merge")
	}
	// decay drops long-dead expressions
	old := []*Expression{{Situation: "x", Style: "y", Count: 0.5, Last: time.Now().Add(-40 * 24 * time.Hour)}}
	if left := decayExpressions(old); len(left) != 0 {
		t.Fatalf("dead expression survived: %v", left[0])
	}
}

func TestClampFloat(t *testing.T) {
	if clampFloat("0.7", 0.3, 0, 1) != 0.7 || clampFloat("2", 0.3, 0, 1) != 0.3 || clampFloat("", 0.3, 0, 1) != 0.3 {
		t.Fatal("clampFloat wrong")
	}
}
