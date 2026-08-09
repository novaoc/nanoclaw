package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
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

// tcgLookup: no set → sets matching query; with set → cards in it (filtered by
// query), each with number, rarity, market price (merged from prices/), and
// the image URL (which the user can ask to have attached via attach_image).
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
		var b strings.Builder
		fmt.Fprintf(&b, "%s sets", game)
		if q != "" {
			fmt.Fprintf(&b, " matching %q", query)
		}
		b.WriteString(":\n")
		n := 0
		for _, s := range sets {
			if q != "" && !strings.Contains(strings.ToLower(s.Name), q) && !strings.Contains(strings.ToLower(s.ID), q) {
				continue
			}
			fmt.Fprintf(&b, "• %s — %s (%d cards, %s)\n", s.ID, s.Name, s.Total, s.ReleaseDate)
			if n++; n >= 25 {
				break
			}
		}
		if n == 0 {
			return "no sets matched — try a broader name, or omit the query to list all."
		}
		b.WriteString("Look up a set's cards by passing its id as `set`.")
		return clip(b.String(), 6000)
	}

	var cards []rbCard
	if err := rbGet(rbData+"/catalog/"+game+"/sets/"+set+".json", &cards); err != nil {
		return "tcg error: couldn't load set " + set + " (" + err.Error() + "). List sets first (omit `set`)."
	}
	var priced struct {
		Prices map[string]float64
	}
	_ = rbGet(rbData+"/prices/"+game+"/latest.json", &priced) // best-effort; prices optional

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
		price := ""
		if p, ok := priced.Prices[strings.ToLower(set)+"-"+rbNormNum(c.Number)]; ok {
			price = fmt.Sprintf(" — $%.2f", p)
		}
		rar := ""
		if c.Rarity != "" {
			rar = " [" + c.Rarity + "]"
		}
		fmt.Fprintf(&b, "• %s #%s%s%s\n  %s\n", c.Name, c.Number, rar, price, c.Image)
		if n++; n >= 15 {
			break
		}
	}
	if n == 0 {
		return "no cards matched in " + set + " — try a different name, or omit the query to list the set."
	}
	b.WriteString("(image URLs shown — offer to attach one if the user wants to see it.)")
	return clip(b.String(), 6000)
}
