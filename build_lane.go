package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// buildLane is the single application-build slot (capacity one). A running
// build holds this lane instead of an ordinary turn slot, so chat / research /
// grok keep moving. /request reads busy() and refuses while it is held.
//
// Release MUST run in a defer — a stuck lane disables /request permanently.
type buildLane struct {
	sem chan struct{} // capacity 1
	mu  sync.Mutex
	// while occupied:
	name   string
	endsAt time.Time // absolute; derived from turnDeadline at acquire
}

func newBuildLane() *buildLane {
	return &buildLane{sem: make(chan struct{}, 1)}
}

// acquire blocks until the lane is free, then records name + absolute end time
// from the turn's real deadline budget. Caller must defer release.
func (bl *buildLane) acquire(name string, deadline time.Duration) {
	bl.sem <- struct{}{}
	bl.mu.Lock()
	bl.name = cleanBuildName(name)
	bl.endsAt = time.Now().Add(deadline)
	bl.mu.Unlock()
}

// tryAcquire is the non-blocking form used to decide whether to show a queue
// react before falling back to a blocking acquire.
func (bl *buildLane) tryAcquire(name string, deadline time.Duration) bool {
	select {
	case bl.sem <- struct{}{}:
		bl.mu.Lock()
		bl.name = cleanBuildName(name)
		bl.endsAt = time.Now().Add(deadline)
		bl.mu.Unlock()
		return true
	default:
		return false
	}
}

// release clears status and frees the slot. Safe to defer across panics.
func (bl *buildLane) release() {
	bl.mu.Lock()
	bl.name = ""
	bl.endsAt = time.Time{}
	bl.mu.Unlock()
	<-bl.sem
}

// setName updates the live label (e.g. when create_rails_app learns the app
// name). No-op if the lane is not held.
func (bl *buildLane) setName(name string) {
	name = cleanBuildName(name)
	if name == "a build" {
		return
	}
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if bl.endsAt.IsZero() {
		return
	}
	bl.name = name
}

// busy reports whether a build is running. remaining is derived from the
// absolute end time set at acquire (turnDeadline is the single source of truth
// for that budget — no second estimate).
func (bl *buildLane) busy() (name string, remaining time.Duration, ok bool) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if bl.endsAt.IsZero() {
		return "", 0, false
	}
	name = bl.name
	if name == "" {
		name = "a build"
	}
	rem := time.Until(bl.endsAt)
	if rem < 0 {
		rem = 0
	}
	return name, rem, true
}

func cleanBuildName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "a build"
	}
	// One line, short — this lands in an ephemeral Discord reply.
	if i := strings.IndexAny(name, "\n\r"); i >= 0 {
		name = name[:i]
	}
	return clip(name, 80)
}

// requestBusyMessage is the ephemeral /request refusal while the build lane
// is held. Names the active build and remaining budget from the tracked
// deadline. Direct voice, no emoji.
func requestBusyMessage(name string, remaining time.Duration) string {
	name = cleanBuildName(name)
	if remaining < 45*time.Second {
		return fmt.Sprintf("I'm mid-build on %s — almost done. Ask again when it finishes.", name)
	}
	mins := int((remaining + time.Minute - 1) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("I'm mid-build on %s — about %d minutes left. Ask again when it finishes.", name, mins)
}
