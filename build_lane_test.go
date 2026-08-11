package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildLaneDoesNotBlockOrdinary(t *testing.T) {
	cfg := testCfg(t)
	cfg.Concurrency = 1
	cfg.Coders = map[string]bool{"dev": true}
	b := &Bot{
		cfg:   cfg,
		locks: make(chan struct{}, cfg.Concurrency),
		build: newBuildLane(),
	}

	// Coder holds the build lane (as a long application build would).
	releaseBuild, queued := b.takeLane("dev", "cafe-app", 20, 1)
	if queued {
		t.Fatal("build lane should be free at start")
	}
	defer releaseBuild()

	// Ordinary turn still takes its own slot immediately.
	done := make(chan struct{})
	go func() {
		rel, q := b.takeLane("rando", "hey", 8, 1)
		if q {
			t.Error("ordinary lane must not wait on a build")
		}
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ordinary turn blocked while build lane occupied")
	}

	name, _, ok := b.build.busy()
	if !ok {
		t.Fatal("build lane should still report busy")
	}
	if !strings.Contains(name, "cafe-app") {
		t.Fatalf("busy name=%q", name)
	}
}

func TestRequestRefusedWhileBuildRuns(t *testing.T) {
	bl := newBuildLane()
	dl := turnDeadline(20, 1, true)
	bl.acquire("driftline-coffee", dl)
	defer bl.release()

	name, rem, ok := bl.busy()
	if !ok {
		t.Fatal("expected busy")
	}
	if name != "driftline-coffee" {
		t.Fatalf("name=%q", name)
	}
	// Remaining must come from the tracked deadline, not a hardcoded constant.
	if rem <= 0 || rem > dl {
		t.Fatalf("remaining %s not derived from deadline %s", rem, dl)
	}
	msg := requestBusyMessage(name, rem)
	if strings.ContainsAny(msg, "😀🎉✅⚡🔧🚀📌⏳🚫") {
		t.Fatalf("emoji in refusal: %q", msg)
	}
	if !strings.Contains(msg, "driftline-coffee") {
		t.Fatalf("refusal must name the build: %q", msg)
	}
	if !strings.Contains(msg, "Ask again when it finishes") {
		t.Fatalf("refusal must invite retry: %q", msg)
	}
	if rem >= 45*time.Second && !strings.Contains(msg, "minutes left") {
		t.Fatalf("want remaining minutes in %q", msg)
	}
}

func TestRequestIdleProceeds(t *testing.T) {
	bl := newBuildLane()
	if _, _, ok := bl.busy(); ok {
		t.Fatal("idle lane must not report busy")
	}
}

func TestBuildLaneReleaseSuccessFailurePanic(t *testing.T) {
	bl := newBuildLane()
	dl := turnDeadline(4, 1, true)

	// success path
	bl.acquire("ok-app", dl)
	if _, _, ok := bl.busy(); !ok {
		t.Fatal("held")
	}
	bl.release()
	if _, _, ok := bl.busy(); ok {
		t.Fatal("released after success")
	}

	// failure path (early return still defers release)
	func() {
		bl.acquire("fail-app", dl)
		defer bl.release()
	}()
	if _, _, ok := bl.busy(); ok {
		t.Fatal("released after failure")
	}
	if !bl.tryAcquire("next", dl) {
		t.Fatal("lane stuck after failure release")
	}
	bl.release()

	// panic path
	func() {
		defer func() { _ = recover() }()
		bl.acquire("panic-app", dl)
		defer bl.release()
		panic("boom")
	}()
	if _, _, ok := bl.busy(); ok {
		t.Fatal("released after panic")
	}
	if !bl.tryAcquire("after-panic", dl) {
		t.Fatal("lane stuck after panic")
	}
	bl.release()
}

func TestBuildLaneSetNameUpdatesBusyLabel(t *testing.T) {
	bl := newBuildLane()
	bl.acquire("go ahead and build it", turnDeadline(20, 1, true))
	defer bl.release()
	bl.setName("notify-cafe")
	name, _, ok := bl.busy()
	if !ok || name != "notify-cafe" {
		t.Fatalf("setName did not stick: ok=%v name=%q", ok, name)
	}
}

func TestNotifyBuildStartedSetsLaneName(t *testing.T) {
	bl := newBuildLane()
	bl.acquire("scaffold please", turnDeadline(20, 1, true))
	defer bl.release()
	tc := &ToolCtx{
		deadline:     turnDeadline(20, 1, true),
		setBuildName: bl.setName,
	}
	tc.notifyBuildStarted("shaped-app")
	name, _, ok := bl.busy()
	if !ok || name != "shaped-app" {
		t.Fatalf("notifyBuildStarted should refresh lane label: %q", name)
	}
}

func TestRequestBusyMessageUsesRemaining(t *testing.T) {
	short := requestBusyMessage("x", 20*time.Second)
	if !strings.Contains(short, "almost done") {
		t.Fatalf("short: %q", short)
	}
	rem := 17*time.Minute + 30*time.Second
	long := requestBusyMessage("y", rem)
	want := int((rem + time.Minute - 1) / time.Minute)
	needle := fmt.Sprintf("about %d minutes left", want)
	if !strings.Contains(long, needle) {
		t.Fatalf("long remaining not reflected: %q want %q", long, needle)
	}
}

func TestDefaultOrdinaryConcurrencyConservative(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	t.Setenv("DISCORD_TOKEN", "d")
	t.Setenv("DEEPSEEK_API_KEY", "k")
	t.Setenv("NANOCLAW_CONCURRENCY", "")
	t.Setenv("VELA_CONCURRENCY", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 1 {
		t.Fatalf("ordinary lane default must stay 1 (RAM-safe), got %d", cfg.Concurrency)
	}
}

func TestTakeLaneCoderUsesBuildNotOrdinary(t *testing.T) {
	cfg := testCfg(t)
	cfg.Concurrency = 1
	cfg.Coders = map[string]bool{"dev": true}
	b := &Bot{
		cfg:   cfg,
		locks: make(chan struct{}, cfg.Concurrency),
		build: newBuildLane(),
	}
	// Fill ordinary lane.
	b.locks <- struct{}{}
	// Coder must still acquire (build lane is separate).
	done := make(chan struct{})
	go func() {
		rel, q := b.takeLane("dev", "app", 20, 1)
		if q {
			t.Error("coder should not queue on ordinary locks")
		}
		if _, _, ok := b.build.busy(); !ok {
			t.Error("coder should occupy build lane")
		}
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("coder blocked on ordinary lane")
	}
	<-b.locks
}
