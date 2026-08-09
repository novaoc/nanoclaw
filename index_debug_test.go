package main

import (
	"os"
	"sort"
	"testing"
	"time"
)

// Diagnostic: BENCH_INDEX_DEBUG=one-piece go test -run TestIndexDebug -v
func TestIndexDebug(t *testing.T) {
	game := os.Getenv("BENCH_INDEX_DEBUG")
	if game == "" {
		t.Skip()
	}
	var sets []rbSet
	if err := rbGet(rbData+"/catalog/"+game+"/sets.json", &sets); err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].ReleaseDate > sets[j].ReleaseDate })
	endDay := int(time.Now().Unix() / 86400)
	startDay := endDay - 90
	for i, s := range sets {
		if i >= indexFetchBudget {
			break
		}
		var doc histDoc
		if err := rbGet(histFileURL(game, s.ID), &doc); err != nil || len(doc.Cards) == 0 {
			t.Logf("skip %-10s %-42s (%s): no history", s.ID, s.Name, s.ReleaseDate)
			continue
		}
		sum := make([]float64, 91)
		n := accumulateIndex(&doc, startDay, endDay, sum)
		t.Logf("set  %-10s %-42s (%s): %d tracked keys, %d qualified", s.ID, s.Name, s.ReleaseDate, len(doc.Cards), n)
	}
	pts, nSets, nCards, dated, err := marketIndex(game, 90)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TOTAL sets=%d cards=%d dated=%v start=%.2f end=%.2f", nSets, nCards, dated,
		pts[0].Price, pts[len(pts)-1].Price)
}
