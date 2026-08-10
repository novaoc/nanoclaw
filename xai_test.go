package main

import (
	"encoding/json"
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

// The live API reports status "done" (not the documented "completed") — a
// regression here means every video "times out" after rendering fine.
func TestVideoOutcomeDoneStatus(t *testing.T) {
	vs := videoStatus{Status: "done"}
	vs.Video.URL = "https://vidgen.x.ai/clip.mp4"
	u, msg, terminal := videoOutcome(vs)
	if !terminal || u != "https://vidgen.x.ai/clip.mp4" || msg != "" {
		t.Fatalf("done status: url=%q msg=%q terminal=%v", u, msg, terminal)
	}
	// unknown status but a URL already present — take it rather than time out
	vs2 := videoStatus{Status: "finalizing"}
	vs2.Video.URL = "https://vidgen.x.ai/late.mp4"
	if u, _, terminal := videoOutcome(vs2); !terminal || u == "" {
		t.Fatal("unknown status with URL should be terminal")
	}
	// still rendering — keep polling
	if _, _, terminal := videoOutcome(videoStatus{Status: "generating"}); terminal {
		t.Fatal("generating should not be terminal")
	}
	if _, msg, terminal := videoOutcome(videoStatus{Status: "failed", Error: "moderation"}); !terminal || msg == "" {
		t.Fatal("failed should be terminal with a message")
	}
}

// image_url is snake_case: without an explicit json tag it silently
// unmarshals to "" (Go matches case-insensitively but not across underscores).
func TestToolArgsImageURLUnmarshal(t *testing.T) {
	var a toolArgs
	if err := json.Unmarshal([]byte(`{"prompt":"a cat","image_url":"https://cdn.discordapp.com/x.png"}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.ImageURL != "https://cdn.discordapp.com/x.png" {
		t.Fatalf("image_url not unmarshaled: %q", a.ImageURL)
	}
}

// data: reference images must pass through inlineImage untouched.
func TestInlineImageDataURLPassthrough(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	in := "data:image/png;base64,AAAA"
	if got := tc.inlineImage(in); got != in {
		t.Fatalf("data URL rewritten: %q", got)
	}
}
