package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// TCG lookup over rarebox-data (github.com/novaoc/rarebox-data): the open,
// daily-refreshed catalog + price dataset. Cards, sets, and market prices for
// Pokémon (EN/JP), Magic, Yu-Gi-Oh!, Lorcana, One Piece (EN/JP), Riftbound.
// Read-only public JSON — no key, CORS-open, docs at docs.rarebox.io/data.

const rbData = "https://raw.githubusercontent.com/novaoc/rarebox-data/main"

var tcgGames = map[string]bool{
	"pokemon": true, "pokemon-ja": true, "mtg": true, "yugioh": true,
	"lorcana": true, "one-piece": true, "one-piece-ja": true, "riftbound": true,
}

var numLetterPrefix = regexp.MustCompile(`^([a-z]+)0*(\d.*)$`)

// rbNormNum mirrors the dataset's number normalization (SCHEMA.md): lowercase,
// drop the "/total" suffix and leading zeros; "001/217"→"1", "TG07"→"tg7".
func rbNormNum(s string) string {
	n := strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[:i]
	}
	trimmed := strings.TrimLeft(n, "0")
	if trimmed == "" {
		return "0"
	}
	n = trimmed
	if m := numLetterPrefix.FindStringSubmatch(n); m != nil {
		n = m[1] + m[2]
	}
	return n
}

func rbGet(url string, v any) error {
	resp, err := ssrfClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type rbSet struct {
	ID, Name, Series, ReleaseDate string
	Total                         int
}
type rbCard struct {
	ID, Name, Number, Rarity, Image string
	Set                             struct{ ID, Name string }
}

// rbPrices loads a game's flat price map (set-number → USD). Best-effort:
// prices are optional, so a miss returns an empty map, never an error.
func rbPrices(game string) map[string]float64 {
	var priced struct {
		Prices map[string]float64 `json:"prices"`
	}
	_ = rbGet(rbData+"/prices/"+game+"/latest.json", &priced)
	return priced.Prices
}

// priceStr formats a card's market price if the dataset has one.
func priceStr(prices map[string]float64, set, number string) string {
	if p, ok := prices[strings.ToLower(set)+"-"+rbNormNum(number)]; ok {
		return fmt.Sprintf(" — $%.2f", p)
	}
	return ""
}

func cardLine(b *strings.Builder, c rbCard, price string) {
	rar := ""
	if c.Rarity != "" {
		rar = " [" + c.Rarity + "]"
	}
	fmt.Fprintf(b, "• %s #%s%s%s\n  %s\n", c.Name, c.Number, rar, price, c.Image)
}

// tcgLookup: no set + query → search cards by NAME across the most recent sets
// (so the caller never has to guess a set id) and, if the query also names a
// set, list those sets. With a set id → cards in it (filtered by query). Each
// card carries number, rarity, market price (merged from prices/), and the
// image URL (attachable via attach_image).
func tcgLookup(game, set, query string) string {
	game = strings.ToLower(strings.TrimSpace(game))
	if !tcgGames[game] {
		return "tcg error: unknown game. Options: pokemon, pokemon-ja, mtg, yugioh, lorcana, one-piece, one-piece-ja, riftbound"
	}
	q := strings.ToLower(strings.TrimSpace(query))
	set = strings.TrimSpace(set)

	if set == "" {
		var sets []rbSet
		if err := rbGet(rbData+"/catalog/"+game+"/sets.json", &sets); err != nil {
			return "tcg error: " + err.Error()
		}
		var setMatches []rbSet
		if q != "" {
			for _, s := range sets {
				if strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.ID), q) {
					setMatches = append(setMatches, s)
				}
			}
			// Query didn't name a set → it's almost certainly a card. Search
			// cards by name across the newest sets instead of making the caller
			// guess a set id (the JP-secret-rare trap).
			if len(setMatches) == 0 {
				return tcgCardSearch(game, q, sets)
			}
			sets = setMatches
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s sets", game)
		if q != "" {
			fmt.Fprintf(&b, " matching %q", query)
		}
		b.WriteString(":\n")
		for i, s := range sets {
			if i >= 25 {
				break
			}
			fmt.Fprintf(&b, "• %s — %s (%d cards, %s)\n", s.ID, s.Name, s.Total, s.ReleaseDate)
		}
		b.WriteString("Look up a set's cards by passing its id as `set` — or just pass the card name as `query` to search across sets.")
		return clip(b.String(), 6000)
	}

	var cards []rbCard
	if err := rbGet(rbData+"/catalog/"+game+"/sets/"+set+".json", &cards); err != nil {
		return "tcg error: couldn't load set " + set + " (" + err.Error() + "). List sets first (omit `set`), or pass the card name as `query` to search across sets."
	}
	prices := rbPrices(game)

	var b strings.Builder
	fmt.Fprintf(&b, "%s / %s", game, set)
	if q != "" {
		fmt.Fprintf(&b, " matching %q", query)
	}
	b.WriteString(":\n")
	n := 0
	for _, c := range cards {
		if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
			continue
		}
		cardLine(&b, c, priceStr(prices, set, c.Number))
		if n++; n >= 15 {
			break
		}
	}
	if n == 0 {
		return "no cards matched in " + set + " — try a different name, or omit the query to list the set."
	}
	b.WriteString("(image URLs shown — offer to attach one if the user wants to see it. When several same-named cards exist and rarity is blank, the higher-priced ones are the chase/secret-rare versions.)")
	return clip(b.String(), 6000)
}

// tcgCardSearch scans the most recent sets for cards whose name matches q, so a
// caller can find a card (and its price) without knowing which set it's in.
// Bounded to the newest sets to keep it to a handful of fetches on the board.
func tcgCardSearch(game, q string, sets []rbSet) string {
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].ReleaseDate > sets[j].ReleaseDate })
	prices := rbPrices(game)

	const maxSets = 18
	var b strings.Builder
	n, scanned := 0, 0
	for _, s := range sets {
		if scanned >= maxSets || n >= 20 {
			break
		}
		scanned++
		var cards []rbCard
		if err := rbGet(rbData+"/catalog/"+game+"/sets/"+s.ID+".json", &cards); err != nil {
			continue
		}
		for _, c := range cards {
			if !strings.Contains(strings.ToLower(c.Name), q) {
				continue
			}
			cardLine(&b, c, priceStr(prices, s.ID, c.Number)+" (set "+s.ID+" — "+s.Name+")")
			if n++; n >= 20 {
				break
			}
		}
	}
	if n == 0 {
		return fmt.Sprintf("no cards named %q in the %d most recent %s sets. For an older card, pass its set id as `set`; to browse sets, omit `query`.", q, scanned, game)
	}
	head := fmt.Sprintf("%s cards matching %q (searched the %d newest sets):\n", game, q, scanned)
	tail := "\n(image URLs shown — offer to attach one if they want to see it. When same-named cards repeat with blank rarity, the higher-priced ones are the chase/secret-rare versions.)"
	return clip(head+b.String()+tail, 6000)
}
