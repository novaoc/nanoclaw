package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Market index: a Card Ladder-style, equal-weight index of a game's tracked
// market, computed from rarebox-price-history. The newest sets' per-card
// series are forward-filled onto a daily grid, each card normalized to 100 at
// the window start, and averaged. Base 100, so the line reads as "the market
// is up/down X% over the window". Sets are processed one at a time so peak
// RAM on the board stays at one parsed history file.

const (
	maxIndexSets     = 16  // newest sets in the basket — bounds fetches + RAM
	indexMinPrice    = 1.0 // bulk under $1 would drown the index in noise
	indexBaseGraceD  = 14  // a card may enter up to this many days after start
	indexStaleCutD   = 30  // …and must still be tracked this close to the end
	indexFetchBudget = maxIndexSets + 8
)

type histDoc struct {
	Cards map[string]map[string][][]float64 `json:"cards"`
}

// histFileURL mirrors cardHistory's per-game file naming: lowercase set ids,
// except one-piece and riftbound which use uppercase; one-piece-ja shares the
// one-piece history directory.
func histFileURL(game, setID string) string {
	dir := game
	if dir == "one-piece-ja" {
		dir = "one-piece"
	}
	f := strings.ToLower(setID)
	if dir == "one-piece" || dir == "riftbound" {
		f = strings.ToUpper(setID)
	}
	return priceHistBase + "/" + dir + "/" + f + ".json"
}

// accumulateIndex folds one set's history into the running per-day sums.
// Returns how many cards qualified: baseline price ≥ indexMinPrice available
// by startDay+grace, and still tracked within indexStaleCutD of endDay.
func accumulateIndex(doc *histDoc, startDay, endDay int, sum []float64) int {
	n := 0
	for _, variants := range doc.Cards {
		var pts [][]float64
		for _, s := range variants {
			if len(s) > len(pts) {
				pts = s
			}
		}
		if len(pts) < 2 || int(pts[len(pts)-1][0]) < endDay-indexStaleCutD {
			continue
		}
		// baseline = the card's value AT window start (last point ≤ startDay);
		// the grace window only admits cards whose tracking begins a few days
		// late — their first price is the baseline, never a mid-window point.
		base := 0.0
		if int(pts[0][0]) <= startDay {
			for _, p := range pts {
				if int(p[0]) > startDay {
					break
				}
				base = p[1]
			}
		} else if int(pts[0][0]) <= startDay+indexBaseGraceD {
			base = pts[0][1]
		}
		if base < indexMinPrice {
			continue
		}
		n++
		v, j := base, 0
		for d := startDay; d <= endDay; d++ { // forward-fill onto the daily grid
			for j < len(pts) && int(pts[j][0]) <= d {
				v = pts[j][1]
				j++
			}
			sum[d-startDay] += v / base * 100
		}
	}
	return n
}

// marketIndex fetches the newest tracked sets of a game and returns the
// equal-weight base-100 index series plus (sets, cards) in the basket. dated
// reports whether the catalog carries release dates: without them "newest N"
// is meaningless (one-piece has releaseDate null on every set — sorting was
// silently keeping catalog order and dropping the actual newest sets), so an
// undated catalog is indexed whole, up to the fetch budget.
func marketIndex(game string, days int) (pts []pricePoint, nSets, nCards int, dated bool, err error) {
	var sets []rbSet
	if err := rbGet(rbData+"/catalog/"+game+"/sets.json", &sets); err != nil {
		return nil, 0, 0, false, err
	}
	for _, s := range sets {
		if strings.TrimSpace(s.ReleaseDate) != "" {
			dated = true
			break
		}
	}
	setCap := maxIndexSets
	if dated {
		sort.SliceStable(sets, func(i, j int) bool { return sets[i].ReleaseDate > sets[j].ReleaseDate })
	} else {
		if len(sets) > indexFetchBudget {
			// yugioh: 1000+ sets, none dated — 24 alphabetical sets would be a
			// misleading basket, so refuse rather than mislabel
			return nil, 0, 0, false, fmt.Errorf("can't build a reliable %s index: its catalog has %d sets but no release dates to pick the newest from", game, len(sets))
		}
		setCap = indexFetchBudget
	}

	endDay := int(time.Now().Unix() / 86400) // NTP-synced on the board
	startDay := endDay - days
	sum := make([]float64, days+1)
	for i, s := range sets {
		if nSets >= setCap || i >= indexFetchBudget {
			break
		}
		var doc histDoc
		if err := rbGet(histFileURL(game, s.ID), &doc); err != nil || len(doc.Cards) == 0 {
			continue // set not tracked (yet) — skip, don't fail the index
		}
		if n := accumulateIndex(&doc, startDay, endDay, sum); n > 0 {
			nSets++
			nCards += n
		}
	}
	if nCards == 0 {
		return nil, 0, 0, dated, fmt.Errorf("no usable price history for a %s index", game)
	}
	pts = make([]pricePoint, 0, len(sum))
	for i, v := range sum {
		pts = append(pts, pricePoint{Ms: int64(startDay+i) * 86400000, Price: v / float64(nCards)})
	}
	return pts, nSets, nCards, dated, nil
}

var gameDisplay = map[string]string{
	"pokemon": "Pokémon", "pokemon-ja": "Pokémon (JP)", "mtg": "Magic",
	"yugioh": "Yu-Gi-Oh!", "lorcana": "Lorcana", "one-piece": "One Piece",
	"one-piece-ja": "One Piece (JP)", "riftbound": "Riftbound",
}
