package main

import (
	"testing"
	"time"
)

func TestMarkEventFeedsTheWatchdogClock(t *testing.T) {
	b := &Bot{}
	b.lastEvent.Store(time.Now().Add(-time.Hour).UnixNano())
	if since := b.sinceLastEvent(); since < 59*time.Minute {
		t.Fatalf("stale clock read %s, want ~1h", since)
	}
	b.markEvent(nil, nil)
	if since := b.sinceLastEvent(); since > time.Second {
		t.Fatalf("markEvent did not refresh the clock: %s", since)
	}
}
