package main

import "testing"

// Exactly /dive and /grok — everything else was removed on purpose
// (registerCommands bulk-overwrites, so anything not listed here is deleted
// from the guild, including strays from older builds).
func TestRegisteredCommands(t *testing.T) {
	want := map[string]bool{"dive": true, "grok": true, "reset": true, "request": true}
	cmds := appCommands()
	if len(cmds) != len(want) {
		t.Fatalf("expected %d commands, got %d", len(want), len(cmds))
	}
	for _, c := range cmds {
		if !want[c.Name] {
			t.Fatalf("unexpected command /%s", c.Name)
		}
	}
}
