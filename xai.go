package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// xAI (Grok) image + video generation. OpenAI-compatible image endpoint
// (/v1/images/generations) and an async video endpoint (/v1/videos/generations
// → poll /v1/videos/{id}). Auth is either a SuperGrok / X Premium+ subscription
// via the device-code OAuth flow (see grokauth.go, preferred) or a pay-as-you-
// go XAI_API_KEY — xaiBearer() picks whichever is present. Generated media is
// fetched and attached to the reply (bytes go to Discord, never into the
// model's context), so this isn't an injection vector.

// xaiMediaClient reuses the SSRF-guarded transport but allows longer transfers
// (a 15s video can take a while to download).
var xaiMediaClient = &http.Client{
	Timeout:       120 * time.Second,
	Transport:     ssrfClient.Transport,
	CheckRedirect: ssrfClient.CheckRedirect,
}

const maxVideoBytes = 25 << 20 // Discord non-boosted upload ceiling headroom

// xaiPost issues an authenticated JSON POST to the xAI API and returns the body.
func xaiPost(cfg *Config, path string, payload any) ([]byte, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(cfg.XAIURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.xaiBearer())
	req.Header.Set("Content-Type", "application/json")
	resp, err := xaiMediaClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return raw, resp.StatusCode, nil
}

func xaiErr(raw []byte, status int) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil {
		if m, ok := e.Error.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok {
				return fmt.Sprintf("xAI %d: %s", status, msg)
			}
		}
		if s, ok := e.Error.(string); ok {
			return fmt.Sprintf("xAI %d: %s", status, s)
		}
	}
	return fmt.Sprintf("xAI %d: %.200s", status, raw)
}

// imageResp is the OpenAI-compatible image response: data[].url or b64_json.
type imageResp struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
	} `json:"data"`
}

func (tc *ToolCtx) generateImage(a toolArgs) string {
	if !tc.cfg.XAIEnabled() {
		return "image gen isn't set up yet — an admin needs to run /grok login (SuperGrok/X Premium) or set an XAI_API_KEY."
	}
	if !tc.cfg.ImageAllowed(tc.authorID) {
		return "REFUSED: image generation is limited to an allowlist here (NANOCLAW_IMAGE_USERS), and this user isn't on it."
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return "image gen: give me a prompt for the picture."
	}
	n := a.N
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4 // cap per-request cost
	}
	payload := map[string]any{
		"model": tc.cfg.XAIImageModel, "prompt": prompt, "n": n, "response_format": "url",
	}
	if a.ImageURL != "" { // reference/edit image
		payload["image_url"] = a.ImageURL
	}
	log.Printf("xai image by=%s n=%d prompt=%.80q", tc.authorID, n, prompt)
	raw, st, err := xaiPost(tc.cfg, "/images/generations", payload)
	if err != nil {
		return "image gen error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't generate that image — " + xaiErr(raw, st)
	}
	var out imageResp
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 {
		return "image gen returned nothing usable."
	}
	saved := 0
	for i, d := range out.Data {
		name := fmt.Sprintf("grok-image-%d.png", i+1)
		if d.URL != "" {
			if s := tc.attachRemoteMedia(d.URL, name); strings.HasPrefix(s, "saved:") {
				saved++
			}
		} else if d.B64JSON != "" {
			if data, err := base64.StdEncoding.DecodeString(d.B64JSON); err == nil {
				if s := tc.saveMedia(name, "image/png", data); strings.HasPrefix(s, "saved:") {
					saved++
				}
			}
		}
	}
	if saved == 0 {
		return "generated the image(s) but couldn't attach them (fetch failed)."
	}
	return fmt.Sprintf("Generated %d image(s) — attached to your reply.", saved)
}

// videoStatus is the poll response for an async video job.
type videoStatus struct {
	Status    string `json:"status"` // queued|processing|completed|failed
	RequestID string `json:"request_id"`
	URL       string `json:"url"`
	Video     struct {
		URL string `json:"url"`
	} `json:"video"`
	Error string `json:"error"`
}

func (vs videoStatus) videoURL() string {
	if vs.URL != "" {
		return vs.URL
	}
	return vs.Video.URL
}

func (tc *ToolCtx) generateVideo(a toolArgs) string {
	if !tc.cfg.XAIEnabled() {
		return "video gen isn't set up yet — an admin needs to run /grok login (SuperGrok/X Premium) or set an XAI_API_KEY."
	}
	if !tc.cfg.ImageAllowed(tc.authorID) {
		return "REFUSED: video generation is limited to an allowlist here (NANOCLAW_IMAGE_USERS), and this user isn't on it."
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" && a.ImageURL == "" {
		return "video gen: give me a prompt (or an image to animate)."
	}
	dur := a.Duration
	if dur == "" {
		dur = "5"
	}
	payload := map[string]any{"model": tc.cfg.XAIVideoModel, "prompt": prompt, "duration": dur}
	if a.ImageURL != "" {
		payload["image_url"] = a.ImageURL
	}
	log.Printf("xai video by=%s dur=%s prompt=%.80q", tc.authorID, dur, prompt)
	raw, st, err := xaiPost(tc.cfg, "/videos/generations", payload)
	if err != nil {
		return "video gen error: " + err.Error()
	}
	if st >= 400 {
		return "couldn't start that video — " + xaiErr(raw, st)
	}
	var start videoStatus
	if err := json.Unmarshal(raw, &start); err != nil || start.RequestID == "" {
		return "video gen didn't return a job id."
	}
	// Poll until complete, bounded so a stuck job can't hold the turn forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	url, perr := tc.pollVideo(ctx, start.RequestID)
	if perr != "" {
		return perr
	}
	if s := tc.attachRemoteMedia(url, "grok-video.mp4"); strings.HasPrefix(s, "saved:") {
		return "Generated the video — attached to your reply."
	}
	// too big to attach (Discord cap) — hand over the link instead
	return "Generated the video, but it's over Discord's upload limit to attach — here's the link (may expire): " + url
}

func (tc *ToolCtx) pollVideo(ctx context.Context, id string) (string, string) {
	for {
		select {
		case <-ctx.Done():
			return "", "video gen timed out — the job took too long. Try a shorter clip."
		case <-time.After(4 * time.Second):
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(tc.cfg.XAIURL, "/")+"/videos/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+tc.cfg.xaiBearer())
		resp, err := xaiMediaClient.Do(req)
		if err != nil {
			continue // transient; keep polling within the deadline
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", "video poll failed — " + xaiErr(raw, resp.StatusCode)
		}
		var vs videoStatus
		if json.Unmarshal(raw, &vs) != nil {
			continue
		}
		switch strings.ToLower(vs.Status) {
		case "completed", "succeeded", "success":
			if u := vs.videoURL(); u != "" {
				return u, ""
			}
			return "", "video completed but no URL came back."
		case "failed", "error":
			if vs.Error != "" {
				return "", "video gen failed: " + vs.Error
			}
			return "", "video gen failed."
		}
	}
}

// attachRemoteMedia fetches a generated media URL (SSRF-guarded) and attaches
// it. Returns "saved:…" on success so callers can count. Media bytes go only to
// Discord, never into the model context.
func (tc *ToolCtx) attachRemoteMedia(u, name string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "media error: bad URL"
	}
	resp, err := xaiMediaClient.Get(u)
	if err != nil {
		return "media error: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("media error: http %d", resp.StatusCode)
	}
	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVideoBytes+1))
	if err != nil {
		return "media error: " + err.Error()
	}
	if len(data) > maxVideoBytes {
		return "media error: too large to attach"
	}
	return tc.saveMedia(name, ct, data)
}

var mediaExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/webm": ".webm", "video/quicktime": ".mov",
}

// saveMedia writes generated media to the artifacts dir and queues it for the
// reply. Returns "saved:<path>" on success (callers test the prefix).
func (tc *ToolCtx) saveMedia(name, contentType string, data []byte) string {
	base := unsafeName.ReplaceAllString(filepath.Base(name), "-")
	if !strings.Contains(base, ".") {
		if ext, ok := mediaExt[contentType]; ok {
			base += ext
		}
	}
	path := filepath.Join(tc.cfg.DataDir, "artifacts", fmt.Sprintf("%d-%s", time.Now().UnixNano(), base))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "media error: " + err.Error()
	}
	tc.Artifacts = append(tc.Artifacts, path)
	return "saved:" + path
}
