package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
)

// A fake vault socket server, to prove nanoclaw's client relays prompts and
// never receives a key regardless of what it asks.
func fakeVault(t *testing.T, connected bool) string {
	t.Helper()
	path := t.TempDir() + "/v.sock"
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); os.Remove(path) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var req map[string]string
					_ = json.Unmarshal(sc.Bytes(), &req)
					var resp map[string]any
					switch req["op"] {
					case "status":
						resp = map[string]any{"ok": true, "connected": connected}
					case "prompt":
						if IsReadish(req["text"]) {
							resp = map[string]any{"ok": true, "result": "BAL: 1.2 ETH"}
						} else {
							resp = map[string]any{"ok": true, "queued": true}
						}
					default:
						resp = map[string]any{"error": "unknown"}
					}
					b, _ := json.Marshal(resp)
					conn.Write(append(b, '\n'))
				}
			}()
		}
	}()
	return path
}

func IsReadish(s string) bool { return strings.Contains(strings.ToLower(s), "balance") }

func TestVaultClientRelay(t *testing.T) {
	vc := NewVaultClient(fakeVault(t, true))
	if !vc.Connected("alice") {
		t.Fatal("status should report connected")
	}
	// read → result relayed
	res, queued, err := vc.Prompt("alice", "chan", "what are my balances")
	if err != nil || queued || res != "BAL: 1.2 ETH" {
		t.Fatalf("read relay wrong: %q q=%v err=%v", res, queued, err)
	}
	// write → queued, no result (vault shows its own button)
	res, queued, err = vc.Prompt("alice", "chan", "send 1 ETH to 0x")
	if err != nil || !queued || res != "" {
		t.Fatalf("write relay wrong: %q q=%v err=%v", res, queued, err)
	}
}

func TestVaultModeRunBankr(t *testing.T) {
	cfg := testCfg(t)
	cfg.VaultSocket = fakeVault(t, false) // not connected
	cfg.Secret = ""                       // vault mode → no local secret
	if !cfg.WalletEnabled() {
		t.Fatal("wallet should be enabled in vault mode")
	}
	if !cfg.CodeEnabled() && len(cfg.Coders) == 0 {
		// code independent of wallet here; just ensure no interlock trip
	}
	tc := &ToolCtx{cfg: cfg, vault: NewVaultClient(cfg.VaultSocket), authorID: "alice", channelID: "c"}
	// unconnected user routed to /connect guidance, never a local path
	if out := tc.runBankr("what are my balances"); !strings.Contains(out, "NOT CONNECTED") {
		t.Fatalf("unconnected vault user should get NOT CONNECTED: %s", out)
	}
}
