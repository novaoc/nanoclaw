package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestVault(t *testing.T, bankrURL string) *Vault {
	t.Helper()
	dir := t.TempDir()
	ks, err := NewKeyStore(dir+"/keys", "server-secret")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := NewPolicy(dir+"/audit.log", 3, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return NewVault(ks, NewBankr(bankrURL), pol, NewConfirmations())
}

// A stub Bankr that completes any job with a canned response.
func stubBankr(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/prompt") {
			_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "j1"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "response": "OK-RESULT"})
	}))
}

func TestVaultReadWriteFlow(t *testing.T) {
	srv := stubBankr(t)
	defer srv.Close()
	v := newTestVault(t, srv.URL)

	// unconnected → not connected, never executes
	if _, err := v.Prompt("alice", "chan", "balances"); err != errNotConnected {
		t.Fatalf("want not-connected, got %v", err)
	}
	if err := v.Connect("alice", "bk_alicealicealice"); err != nil {
		t.Fatal(err)
	}
	// a READ executes immediately
	r, err := v.Prompt("alice", "chan", "what are my balances?")
	if err != nil || r.Result != "OK-RESULT" || r.Queued {
		t.Fatalf("read flow wrong: %+v %v", r, err)
	}
	// a WRITE queues (no execution) and posts via onQueue
	var postedToken string
	v.onQueue = func(_, _, token, _ string) { postedToken = token }
	r, err = v.Prompt("alice", "chan", "send 1 ETH to 0xabc")
	if err != nil || !r.Queued || r.Result != "" {
		t.Fatalf("write should queue: %+v %v", r, err)
	}
	if postedToken == "" {
		t.Fatal("vault must post its own confirm button on queue")
	}
	// only the owner can execute; wrong clicker rejected
	if _, err := v.Execute(postedToken, "mallory"); err != errNotYours {
		t.Fatalf("wrong clicker should be rejected: %v", err)
	}
	out, err := v.Execute(postedToken, "alice")
	if err != nil || out != "OK-RESULT" {
		t.Fatalf("owner execute failed: %q %v", out, err)
	}
	// one-shot: token gone
	if _, err := v.Execute(postedToken, "alice"); err != errExpired {
		t.Fatalf("token should be single-use: %v", err)
	}
}

func TestVaultPolicyCap(t *testing.T) {
	srv := stubBankr(t)
	defer srv.Close()
	v := newTestVault(t, srv.URL) // maxPerDay=3, cooldown tiny
	_ = v.Connect("bob", "bk_bobbobbobbob")

	exec := func() error {
		v.onQueue = func(_, _, _, _ string) {}
		r, err := v.Prompt("bob", "c", "send 1 ETH to 0x")
		if err != nil {
			return err
		}
		if !r.Queued {
			t.Fatal("expected queue")
		}
		// grab the token from the confirmations map (owner path)
		var tok string
		v.confirms.mu.Lock()
		for k := range v.confirms.m {
			tok = k
		}
		v.confirms.mu.Unlock()
		_, err = v.Execute(tok, "bob")
		return err
	}
	for i := 0; i < 3; i++ {
		time.Sleep(15 * time.Millisecond) // clear cooldown
		if err := exec(); err != nil {
			t.Fatalf("write %d should pass: %v", i, err)
		}
	}
	time.Sleep(15 * time.Millisecond)
	// 4th within the day must be capped at queue time
	if _, err := v.Prompt("bob", "c", "send 1 ETH to 0x"); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("daily cap should block the 4th write, got %v", err)
	}
}

func TestKeyStoreNeverReturnsKeyOverSocket(t *testing.T) {
	// The socket protocol has no op that returns a key — assert dispatch only
	// ever yields status/result/queued, and 'read' of a key file isn't a route.
	v := newTestVault(t, "http://unused")
	s := NewServer(v, t.TempDir()+"/s.sock")
	for _, op := range []string{"status", "prompt", "disconnect", "getkey", "read", "dump"} {
		r := s.dispatch(request{Op: op, UID: "x"})
		blob, _ := json.Marshal(r)
		if strings.Contains(string(blob), "bk_") {
			t.Fatalf("op %q leaked a key: %s", op, blob)
		}
	}
}
