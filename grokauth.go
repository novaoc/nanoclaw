package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GrokAuth implements xAI's OAuth 2.0 device-code flow (RFC 8628) so Vela can
// use a SuperGrok / X Premium+ subscription for image/video generation WITHOUT
// an API key. The client_id is xAI's public CLI id (not a secret). Tokens are
// stored 0600 on the SD, never enter the model context/history/logs, and are
// sent ONLY to api.x.ai. Refresh tokens rotate on every refresh, so we persist
// the new one each time.

const (
	grokClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	grokScopes      = "openid profile email offline_access grok-cli:access api:access"
	grokDeviceURL   = "https://auth.x.ai/oauth2/device/code"
	grokTokenURL    = "https://auth.x.ai/oauth2/token"
	grokDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

type grokTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds
}

type GrokAuth struct {
	mu   sync.Mutex
	path string
	tok  grokTokens
}

func NewGrokAuth(dataDir string) *GrokAuth {
	g := &GrokAuth{path: filepath.Join(dataDir, "grok-auth.json")}
	if b, err := os.ReadFile(g.path); err == nil {
		_ = json.Unmarshal(b, &g.tok)
	}
	return g
}

func (g *GrokAuth) Connected() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tok.RefreshToken != "" || g.tok.AccessToken != ""
}

func (g *GrokAuth) save() {
	b, err := json.Marshal(g.tok)
	if err != nil {
		return
	}
	tmp := g.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, g.path)
	}
}

// Clear forgets the subscription login.
func (g *GrokAuth) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tok = grokTokens{}
	_ = os.Remove(g.path)
}

// deviceCode is the device-authorization response.
type deviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDevice kicks off the device flow and returns the code/URL to show the
// user. Uses form-encoded POST per RFC 8628.
func (g *GrokAuth) StartDevice() (*deviceCode, error) {
	form := url.Values{"client_id": {grokClientID}, "scope": {grokScopes}}
	raw, st, err := grokForm(grokDeviceURL, form)
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, fmt.Errorf("device code request failed (%d): %.200s", st, raw)
	}
	var dc deviceCode
	if err := json.Unmarshal(raw, &dc); err != nil || dc.DeviceCode == "" {
		return nil, fmt.Errorf("bad device-code response: %.200s", raw)
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

// PollForToken polls the token endpoint until the user approves (or it times
// out / is denied). Blocks; caller runs it in a goroutine. Returns nil on
// success (tokens stored) or an error describing why it stopped.
func (g *GrokAuth) PollForToken(dc *deviceCode) error {
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	interval := time.Duration(dc.Interval) * time.Second
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		form := url.Values{
			"grant_type":  {grokDeviceGrant},
			"device_code": {dc.DeviceCode},
			"client_id":   {grokClientID},
		}
		raw, st, err := grokForm(grokTokenURL, form)
		if err != nil {
			continue // transient; keep polling
		}
		if st == 200 {
			return g.storeTokenResponse(raw)
		}
		// error body: authorization_pending | slow_down | access_denied | expired_token
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		switch e.Error {
		case "authorization_pending":
			// keep waiting
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return fmt.Errorf("you declined the authorization")
		case "expired_token":
			return fmt.Errorf("the code expired before you approved — run it again")
		default:
			if st >= 400 && e.Error != "" {
				return fmt.Errorf("authorization failed: %s", e.Error)
			}
		}
	}
	return fmt.Errorf("timed out waiting for you to approve")
}

func (g *GrokAuth) storeTokenResponse(raw []byte) error {
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || tr.AccessToken == "" {
		return fmt.Errorf("bad token response")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tok.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" { // rotation: keep the newest
		g.tok.RefreshToken = tr.RefreshToken
	}
	exp := tr.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	g.tok.ExpiresAt = time.Now().Unix() + int64(exp) - 60 // refresh a minute early
	g.save()
	return nil
}

// Token returns a valid access token, refreshing if it's expired. Empty string
// (no error path to the model) if not connected or refresh fails.
func (g *GrokAuth) Token() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	valid := g.tok.AccessToken != "" && time.Now().Unix() < g.tok.ExpiresAt
	refresh := g.tok.RefreshToken
	g.mu.Unlock()
	if valid {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.tok.AccessToken
	}
	if refresh == "" {
		return ""
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {grokClientID},
	}
	raw, st, err := grokForm(grokTokenURL, form)
	if err != nil || st != 200 {
		return ""
	}
	if g.storeTokenResponse(raw) != nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tok.AccessToken
}

// grokForm POSTs an application/x-www-form-urlencoded body (SSRF-guarded) and
// returns body + status. Endpoint pinning: only https on *.x.ai.
func grokForm(endpoint string, form url.Values) ([]byte, int, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || !isXaiHost(u.Hostname()) {
		return nil, 0, fmt.Errorf("refusing non-x.ai auth endpoint")
	}
	resp, err := xaiMediaClient.PostForm(endpoint, form)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// isXaiHost matches x.ai and its subdomains only — a dot-boundary check so
// "evilx.ai" and "auth.x.ai.evil.com" are both rejected.
func isXaiHost(host string) bool {
	host = strings.ToLower(host)
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}
