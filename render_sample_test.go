package main

import (
	"os"
	"testing"
)

func TestRenderSampleToFile(t *testing.T) {
	out := os.Getenv("BENCH_SAMPLE_OUT")
	if out == "" {
		t.Skip()
	}
	models := []benchModel{
		{Name: "DeepSeek-V4 Flash", Scores: []*float64{f(84.2), f(68.4), f(57.6), f(46.7), f(89.3)}},
		{Name: "Llama 3.1 405B", Scores: []*float64{f(73.3), f(51.1), f(33.4), nil, f(89.0)}},
		{Name: "GPT-5.2", Scores: []*float64{f(87.1), f(74.9), f(64.0), f(52.8), f(92.4)}},
		{Name: "Claude Opus 5", Scores: []*float64{f(88.0), f(76.2), f(67.1), f(55.0), nil}},
	}
	b := renderBenchPNG("DeepSeek-V4 Flash vs the field",
		"vendor model cards · Aug 2026 · pass@1 where stated",
		[]string{"MMLU-Pro", "GPQA Diamond", "SWE-bench Verified", "AIME 2025", "HumanEval"}, models)
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
