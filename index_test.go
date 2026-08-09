package main

import (
	"math"
	"testing"
)

func TestAccumulateIndex(t *testing.T) {
	start, end := 1000, 1010
	doc := &histDoc{Cards: map[string]map[string][][]float64{
		// doubles over the window: 10 → 20
		"1": {"normal": {{999, 10}, {1005, 15}, {1010, 20}}},
		// flat at 4
		"2": {"holo": {{998, 4}, {1009, 4}}},
		// excluded: baseline under $1
		"3": {"normal": {{999, 0.25}, {1010, 0.5}}},
		// excluded: stale (last point long before the window end)
		"4": {"normal": {{900, 50}, {950, 60}}},
		// excluded: first point after the grace horizon
		"5": {"normal": {{1030, 9}, {1040, 9}}},
	}}
	sum := make([]float64, end-start+1)
	n := accumulateIndex(doc, start, end, sum)
	if n != 2 {
		t.Fatalf("qualified cards = %d, want 2", n)
	}
	// day 0: both cards at baseline → mean 100
	if got := sum[0] / float64(n); math.Abs(got-100) > 1e-9 {
		t.Errorf("index start = %v, want 100", got)
	}
	// day 10: card1 at 200, card2 at 100 → mean 150
	if got := sum[10] / float64(n); math.Abs(got-150) > 1e-9 {
		t.Errorf("index end = %v, want 150", got)
	}
	// forward-fill between points: day 3 card1 still at 10 → 100; mean 100
	if got := sum[3] / float64(n); math.Abs(got-100) > 1e-9 {
		t.Errorf("index day3 = %v, want 100 (forward-fill)", got)
	}
}

func TestHistFileURL(t *testing.T) {
	cases := map[[2]string]string{
		{"pokemon", "SV8"}:      priceHistBase + "/pokemon/sv8.json",
		{"one-piece", "op-01"}:  priceHistBase + "/one-piece/OP-01.json",
		{"one-piece-ja", "op1"}: priceHistBase + "/one-piece/OP1.json",
		{"riftbound", "ogn"}:    priceHistBase + "/riftbound/OGN.json",
	}
	for in, want := range cases {
		if got := histFileURL(in[0], in[1]); got != want {
			t.Errorf("histFileURL(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
