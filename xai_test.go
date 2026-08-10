package main

import (
	"strings"
	"testing"
)

func TestXAIDisabledMessages(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)} // no XAI key
	if out := tc.Run("generate_image", `{"prompt":"a cat"}`); !strings.Contains(out, "isn't set up") {
		t.Errorf("image gen without key: %q", out)
	}
	if out := tc.Run("generate_video", `{"prompt":"a cat"}`); !strings.Contains(out, "isn't set up") {
		t.Errorf("video gen without key: %q", out)
	}
}

func TestImageToolsHiddenWithoutKey(t *testing.T) {
	// the tool defs must NOT advertise image/video when xAI is off
	defs := toolDefs(testCfg(t))
	for _, d := range defs {
		if d.Function.Name == "generate_image" || d.Function.Name == "generate_video" {
			t.Fatalf("%s offered without XAI_API_KEY", d.Function.Name)
		}
	}
	cfg := testCfg(t)
	cfg.XAIKey = "test-key"
	cfg.XAIImageModel = "grok-imagine-image-quality"
	got := map[string]bool{}
	for _, d := range toolDefs(cfg) {
		got[d.Function.Name] = true
	}
	if !got["generate_image"] || !got["generate_video"] {
		t.Fatal("image/video tools missing when XAI_API_KEY is set")
	}
}

func TestImageAllowlistGate(t *testing.T) {
	cfg := testCfg(t)
	cfg.XAIKey = "test-key"
	cfg.ImageUsers = map[string]bool{"vip": true}
	tc := &ToolCtx{cfg: cfg, authorID: "rando"}
	if out := tc.generateImage(toolArgs{Prompt: "a cat"}); !strings.Contains(out, "REFUSED") {
		t.Errorf("non-allowlisted user should be refused: %q", out)
	}
}

func TestXAIErr(t *testing.T) {
	raw := []byte(`{"error":{"message":"insufficient credits"}}`)
	if got := xaiErr(raw, 402); !strings.Contains(got, "insufficient credits") {
		t.Errorf("xaiErr = %q", got)
	}
	if got := xaiErr([]byte(`{"error":"bad key"}`), 401); !strings.Contains(got, "bad key") {
		t.Errorf("xaiErr string form = %q", got)
	}
}

func TestSaveMediaTypesAndExt(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	// video with no extension in name → gets .mp4 from content-type
	out := tc.saveMedia("grok-video", "video/mp4", []byte("fakebytes"))
	if !strings.HasPrefix(out, "saved:") || !strings.HasSuffix(out, ".mp4") {
		t.Fatalf("saveMedia video: %q", out)
	}
	if len(tc.Artifacts) != 1 {
		t.Fatalf("artifact not queued: %v", tc.Artifacts)
	}
}

func TestVideoURLExtraction(t *testing.T) {
	if (videoStatus{URL: "https://x/a.mp4"}).videoURL() != "https://x/a.mp4" {
		t.Error("top-level url")
	}
	vs := videoStatus{}
	vs.Video.URL = "https://x/b.mp4"
	if vs.videoURL() != "https://x/b.mp4" {
		t.Error("nested video.url")
	}
}
