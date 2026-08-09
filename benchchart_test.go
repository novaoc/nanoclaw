package main

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestBenchChartRender(t *testing.T) {
	models := []benchModel{
		{Name: "DeepSeek-V4", Scores: []*float64{f(88.5), f(71.2), nil}},
		{Name: "Llama 3.1 405B", Scores: []*float64{f(85.1), nil, f(49.0)}},
	}
	b := renderBenchPNG("Test chart", "test · Aug 2026", []string{"MMLU-Pro", "GPQA Diamond", "SWE-bench Verified"}, models)
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a decodable PNG: %v", err)
	}
	if img.Bounds().Dx() != 1600 || img.Bounds().Dy() != 900 {
		t.Fatalf("unexpected size %v", img.Bounds())
	}
}

func TestBenchChartRenderSingleModelBigScale(t *testing.T) {
	// one model (no legend), a >100 score (Elo-style scale), a zero score
	models := []benchModel{{Name: "Solo", Scores: []*float64{f(1287), f(0)}}}
	b := renderBenchPNG("Solo", "arena · Aug 2026", []string{"Arena Elo", "Zeroed"}, models)
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("not a decodable PNG: %v", err)
	}
}

func TestBenchChartToolValidation(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	cases := []struct{ args, wantSub string }{
		{`{"benchmarks":[],"models":[]}`, "need benchmarks"},
		{`{"benchmarks":["MMLU"],"models":[{"name":"A","scores":[1,2]}]}`, "align them"},
		{`{"benchmarks":["MMLU"],"models":[{"name":"","scores":[5]}]}`, "needs a name"},
		{`{"benchmarks":["MMLU"],"models":[{"name":"A","scores":[-3]}]}`, "bad score"},
	}
	for _, c := range cases {
		if out := tc.Run("bench_chart", c.args); !strings.Contains(out, c.wantSub) {
			t.Errorf("args %s: got %q, want substring %q", c.args, out, c.wantSub)
		}
	}
}

func TestBenchChartToolDispatch(t *testing.T) {
	tc := &ToolCtx{cfg: testCfg(t)}
	// null scores must unmarshal and render; result should attach an artifact
	out := tc.Run("bench_chart", `{"title":"A vs B","source":"cards · Aug 2026",
		"benchmarks":["MMLU-Pro","GPQA"],
		"models":[{"name":"A","scores":[88.5,null]},{"name":"B","scores":[81.0,60.2]}]}`)
	if !strings.Contains(out, "attached") {
		t.Fatalf("unexpected result: %q", out)
	}
	if len(tc.Artifacts) != 1 || !strings.HasSuffix(tc.Artifacts[0], ".png") {
		t.Fatalf("expected one png artifact, got %v", tc.Artifacts)
	}
	b, err := os.ReadFile(tc.Artifacts[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("artifact not a decodable PNG: %v", err)
	}
	// bench_chart is neither web nor code: it must not trip the injection guard
	if tc.usedWeb || tc.usedCode {
		t.Fatal("bench_chart must not mark the turn as web or code")
	}
}
