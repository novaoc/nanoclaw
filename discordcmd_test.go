package main

import "testing"

// Only /grok remains as a slash command — everything else was removed on
// purpose (registerCommands bulk-overwrites, so anything not listed here is
// deleted from the guild, including strays from older builds).
func TestOnlyGrokCommandRegistered(t *testing.T) {
	cmds := appCommands()
	if len(cmds) != 1 || cmds[0].Name != "grok" {
		names := make([]string, 0, len(cmds))
		for _, c := range cmds {
			names = append(names, c.Name)
		}
		t.Fatalf("expected exactly [grok], got %v", names)
	}
}
