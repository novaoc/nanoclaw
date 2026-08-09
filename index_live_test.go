package main

import (
	"fmt"
	"os"
	"testing"
)

// Live smoke test (network): BENCH_INDEX_OUT=/path/x.png go test -run TestMarketIndexLive
func TestMarketIndexLive(t *testing.T) {
	out := os.Getenv("BENCH_INDEX_OUT")
	if out == "" {
		t.Skip()
	}
	pts, nSets, nCards, err := marketIndex("pokemon", 90)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sets=%d cards=%d points=%d start=%.1f end=%.1f", nSets, nCards, len(pts), pts[0].Price, pts[len(pts)-1].Price)
	sub := fmt.Sprintf("rarebox · newest %d sets · %d cards ≥ $1 · equal-weight · base 100 at window start", nSets, nCards)
	b := renderChartPNG("Pokémon market index", sub, pts, func(v float64) string { return fmt.Sprintf("%.1f", v) })
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
