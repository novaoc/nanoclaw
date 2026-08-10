package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestGrokEndpointPinning(t *testing.T) {
	// only https on *.x.ai may receive the auth POST
	for _, bad := range []string{
		"http://auth.x.ai/oauth2/token",    // not https
		"https://evil.com/oauth2/token",    // wrong host
		"https://auth.x.ai.evil.com/token", // suffix-trick host
		"https://evilx.ai/oauth2/token",    // no dot boundary
	} {
		if _, _, err := grokForm(bad, url.Values{}); err == nil {
			t.Errorf("grokForm accepted a non-x.ai endpoint: %s", bad)
		}
	}
	// the real hosts must pass the host check
	for _, ok := range []string{"x.ai", "auth.x.ai", "api.x.ai"} {
		if !isXaiHost(ok) {
			t.Errorf("isXaiHost rejected a real host: %s", ok)
		}
	}
}

func TestGrokAuthLifecycle(t *testing.T) {
	dir := t.TempDir()
	g := NewGrokAuth(dir)
	if g.Connected() {
		t.Fatal("fresh store should not be connected")
	}
	// simulate a successful token response landing in the store
	if err := g.storeTokenResponse([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3600}`)); err != nil {
		t.Fatal(err)
	}
	if !g.Connected() {
		t.Fatal("should be connected after token store")
	}
	if g.Token() != "AT" {
		t.Errorf("Token() = %q, want AT", g.Token())
	}
	// persisted at 0600 and reloads
	fi, err := os.Stat(filepath.Join(dir, "grok-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("grok-auth perms = %v, want 0600", fi.Mode().Perm())
	}
	if g2 := NewGrokAuth(dir); !g2.Connected() {
		t.Error("reloaded store lost the login")
	}
	// refresh-token rotation: a refresh response with a new RT replaces it
	g.storeTokenResponse([]byte(`{"access_token":"AT2","refresh_token":"RT2","expires_in":3600}`))
	g.mu.Lock()
	if g.tok.RefreshToken != "RT2" {
		t.Errorf("refresh token not rotated: %q", g.tok.RefreshToken)
	}
	g.mu.Unlock()
	// clear wipes memory + disk
	g.Clear()
	if g.Connected() {
		t.Error("should be disconnected after Clear")
	}
	if _, err := os.Stat(filepath.Join(dir, "grok-auth.json")); !os.IsNotExist(err) {
		t.Error("grok-auth.json should be gone after Clear")
	}
}

func TestNilGrokAuthSafe(t *testing.T) {
	var g *GrokAuth
	if g.Connected() || g.Token() != "" {
		t.Error("nil GrokAuth must behave as disconnected")
	}
}

func TestXAIEnabledViaOAuth(t *testing.T) {
	cfg := testCfg(t)
	cfg.Grok = NewGrokAuth(t.TempDir())
	if cfg.XAIEnabled() {
		t.Fatal("not enabled before login")
	}
	cfg.Grok.storeTokenResponse([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":3600}`))
	if !cfg.XAIEnabled() {
		t.Fatal("OAuth login should enable xAI")
	}
	if cfg.xaiBearer() != "AT" {
		t.Errorf("bearer = %q, want the OAuth token", cfg.xaiBearer())
	}
}
